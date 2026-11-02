// Package buildtest holds the fakes build tests run against: a sandboxd
// that speaks the real wire protocol and runs attached processes on the
// host, and fakeclaude, a stand-in for the CLI that speaks its stdio
// permission protocol.
package buildtest

import (
	"archive/tar"
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/sandbox"
)

var (
	cliOnce sync.Once
	cliPath string
	cliErr  error
)

// FakeCLI builds testdata/fakeclaude once per test binary and returns its
// path.
func FakeCLI(t testing.TB) string {
	t.Helper()
	cliOnce.Do(func() {
		dir, err := os.MkdirTemp("", "orbit-fakeclaude")
		if err != nil {
			cliErr = err
			return
		}
		_, here, _, _ := runtime.Caller(0)
		cliPath = filepath.Join(dir, "fakeclaude")
		cmd := exec.Command("go", "build", "-o", cliPath, ".")
		cmd.Dir = filepath.Join(filepath.Dir(here), "testdata", "fakeclaude")
		if b, err := cmd.CombinedOutput(); err != nil {
			cliErr = &buildErr{err, string(b)}
		}
	})
	if cliErr != nil {
		t.Fatal(cliErr)
	}
	return cliPath
}

type buildErr struct {
	err error
	out string
}

func (e *buildErr) Error() string { return "build fakeclaude: " + e.err.Error() + "\n" + e.out }

// Attaches returns every attach request the fake received.
func (f *Sandboxd) Attaches() []sandbox.AttachRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sandbox.AttachRequest(nil), f.attaches...)
}

// Tar writes dir as a tar of files, directories and symlinks.
func Tar(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h := &tar.Header{Name: filepath.ToSlash(rel), Mode: int64(info.Mode().Perm()), ModTime: info.ModTime()}
		switch {
		case d.IsDir():
			h.Typeflag, h.Name = tar.TypeDir, h.Name+"/"
		case info.Mode()&fs.ModeSymlink != 0:
			h.Typeflag = tar.TypeSymlink
			h.Linkname, _ = os.Readlink(p)
		case info.Mode().IsRegular():
			h.Typeflag, h.Size = tar.TypeReg, info.Size()
		default:
			return nil
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag == tar.TypeReg {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// Git runs git in dir with no user configuration, failing the test on an
// error.
func Git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Person", "GIT_AUTHOR_EMAIL=p@example.com", "GIT_COMMITTER_NAME=Person", "GIT_COMMITTER_EMAIL=p@example.com")
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

// Repo makes a bare repository standing in for GitHub's git side, seeded
// with orbit's shape on main: code, a workflow, a deploy directory.
func Repo(t testing.TB) string {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "orbit.git")
	Git(t, root, "init", "--quiet", "--bare", bare)
	seed := filepath.Join(root, "seed")
	Git(t, root, "init", "--quiet", seed)
	for name, body := range map[string]string{
		"go.mod":                   "module example.com/x\n",
		"README.md":                "# x\n",
		"main.go":                  "package main\n\nfunc main() {}\n",
		".github/workflows/ci.yml": "on: push\n",
		"deploy/Dockerfile":        "FROM scratch\n",
	} {
		p := filepath.Join(seed, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	Git(t, seed, "add", ".")
	Git(t, seed, "commit", "--quiet", "-m", "seed")
	Git(t, seed, "push", "--quiet", bare, "HEAD:refs/heads/main")
	Git(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return bare
}

// RevParse returns ref's commit in repo, or "" when it does not exist.
func RevParse(repo, ref string) string {
	out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "--quiet", ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// SandboxToken is the fake's bearer token.
const SandboxToken = "sandbox-token-sandbox-token-sandbox-token"

// Sandboxd serves sandboxd's API from a directory per sandbox and runs
// attached processes on the host, mapping argv[0] "claude" to fakeclaude.
// It speaks the real wire protocol, so orbit's real client talks to it.
// Sandboxd serves sandboxd's API from a directory per sandbox.
type Sandboxd struct {
	t    *testing.T
	srv  *httptest.Server
	root string

	mu        sync.Mutex
	dirs      map[string]string
	created   []string
	destroyed []string
	attaches  []sandbox.AttachRequest
}

// NewSandboxd starts a fake sandboxd, stopped with the test.
func NewSandboxd(t *testing.T) *Sandboxd {
	FakeCLI(t)
	f := &Sandboxd{t: t, root: t.TempDir(), dirs: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", f.create)
	mux.HandleFunc("POST /v1/sandboxes/{id}/attach", f.attach)
	mux.HandleFunc("GET /v1/sandboxes/{id}/archive", f.archive)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", f.destroy)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+SandboxToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// Client is orbit's real client for the fake.
func (f *Sandboxd) Client(t *testing.T) *sandbox.Client {
	c, err := sandbox.NewClient(strings.TrimPrefix(f.srv.URL, "http://"), SandboxToken)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Dir is a live sandbox's directory.
func (f *Sandboxd) Dir(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.dirs[id]
	return d, ok
}

func (f *Sandboxd) create(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	id := hex.EncodeToString(b)
	dir := filepath.Join(f.root, id[:12])
	if err := untar(dir, r.Body); err != nil {
		http.Error(w, `{"error":"bad tar"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.dirs[id] = dir
	f.created = append(f.created, r.URL.Query().Get("run_id"))
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(sandbox.CreateResponse{ID: id, ExpiresAt: time.Now().Add(time.Hour)})
}

func (f *Sandboxd) attach(w http.ResponseWriter, r *http.Request) {
	dir, ok := f.Dir(r.PathValue("id"))
	if !ok {
		http.Error(w, `{"error":"no such sandbox"}`, http.StatusNotFound)
		return
	}
	var ar sandbox.AttachRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ar); err != nil || ar.Validate() != nil {
		http.Error(w, `{"error":"bad attach"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.attaches = append(f.attaches, ar)
	f.mu.Unlock()
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + sandbox.AttachUpgrade + "\r\n\r\n")
	_ = buf.Flush()
	bin := ar.Argv[0]
	if bin == "claude" {
		bin = FakeCLI(f.t)
	}
	cmd := exec.Command(bin, ar.Argv[1:]...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
	for k, v := range ar.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, _ := cmd.StdinPipe()
	var mu sync.Mutex
	frame := func(typ byte) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			return len(p), sandbox.WriteChunked(conn, typ, p)
		})
	}
	cmd.Stdout, cmd.Stderr = frame(sandbox.FrameStdout), frame(sandbox.FrameStderr)
	if err := cmd.Start(); err != nil {
		b, _ := json.Marshal(sandbox.Exit{ExitCode: -1, Error: err.Error()})
		_ = sandbox.WriteFrame(conn, sandbox.FrameExit, b)
		return
	}
	go func() {
		_, _ = io.Copy(stdin, bufio.NewReader(io.MultiReader(buf.Reader, conn)))
		_ = stdin.Close()
	}()
	err = cmd.Wait()
	ex := sandbox.Exit{}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		ex.ExitCode = ee.ExitCode()
	}
	b, _ := json.Marshal(ex)
	mu.Lock()
	_ = sandbox.WriteFrame(conn, sandbox.FrameExit, b)
	mu.Unlock()
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func (f *Sandboxd) archive(w http.ResponseWriter, r *http.Request) {
	dir, ok := f.Dir(r.PathValue("id"))
	if !ok {
		http.Error(w, `{"error":"no such sandbox"}`, http.StatusNotFound)
		return
	}
	if err := Tar(w, dir); err != nil {
		http.Error(w, `{"error":"pack"}`, http.StatusInternalServerError)
	}
}

func (f *Sandboxd) destroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	dir, ok := f.dirs[id]
	delete(f.dirs, id)
	if ok {
		f.destroyed = append(f.destroyed, id)
	}
	f.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	_ = os.RemoveAll(dir)
	w.WriteHeader(http.StatusNoContent)
}

// Counts reports sandboxes created, destroyed and still open.
func (f *Sandboxd) Counts() (created, destroyed, open int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created), len(f.destroyed), len(f.dirs)
}

// untar unpacks a tar the way sandboxd's filter would let it through.
func untar(dir string, r io.Reader) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		p := filepath.Join(dir, filepath.FromSlash(h.Name))
		switch h.Typeflag {
		case tar.TypeDir:
			_ = os.MkdirAll(p, 0o755)
		case tar.TypeReg:
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)|0o600)
			if err != nil {
				return err
			}
			_, _ = io.Copy(f, tr)
			_ = f.Close()
		case tar.TypeSymlink:
			_ = os.Symlink(h.Linkname, p)
		}
	}
}

// URL is the fake's base URL.
func (f *Sandboxd) URL() string { return f.srv.URL }
