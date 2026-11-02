package build

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/sandbox"
)

// fakeCLI is the path of the compiled testdata/fakeclaude.
var fakeCLI string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "orbit-build-test")
	if err != nil {
		panic(err)
	}
	_, here, _, _ := runtime.Caller(0)
	fakeCLI = filepath.Join(dir, "fakeclaude")
	cmd := exec.Command("go", "build", "-o", fakeCLI, ".")
	cmd.Dir = filepath.Join(filepath.Dir(here), "testdata", "fakeclaude")
	if b, err := cmd.CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build fakeclaude: %v\n%s", err, b))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

const sandboxToken = "sandbox-token-sandbox-token-sandbox-token"

// fakeSandboxd serves sandboxd's API from a directory per sandbox and runs
// attached processes on the host, mapping argv[0] "claude" to fakeclaude.
// It speaks the real wire protocol, so orbit's real client talks to it.
type fakeSandboxd struct {
	t    *testing.T
	srv  *httptest.Server
	root string

	mu        sync.Mutex
	dirs      map[string]string
	created   []string
	destroyed []string
	attaches  []sandbox.AttachRequest
}

func newFakeSandboxd(t *testing.T) *fakeSandboxd {
	f := &fakeSandboxd{t: t, root: t.TempDir(), dirs: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", f.create)
	mux.HandleFunc("POST /v1/sandboxes/{id}/attach", f.attach)
	mux.HandleFunc("GET /v1/sandboxes/{id}/archive", f.archive)
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", f.destroy)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+sandboxToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSandboxd) client(t *testing.T) *sandbox.Client {
	c, err := sandbox.NewClient(strings.TrimPrefix(f.srv.URL, "http://"), sandboxToken)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *fakeSandboxd) dir(id string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d, ok := f.dirs[id]
	return d, ok
}

func (f *fakeSandboxd) create(w http.ResponseWriter, r *http.Request) {
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

func (f *fakeSandboxd) attach(w http.ResponseWriter, r *http.Request) {
	dir, ok := f.dir(r.PathValue("id"))
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
		bin = fakeCLI
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
	if ee, ok := err.(*exec.ExitError); ok {
		ex.ExitCode = ee.ExitCode()
	}
	b, _ := json.Marshal(ex)
	mu.Lock()
	_ = sandbox.WriteFrame(conn, sandbox.FrameExit, b)
	mu.Unlock()
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func (f *fakeSandboxd) archive(w http.ResponseWriter, r *http.Request) {
	dir, ok := f.dir(r.PathValue("id"))
	if !ok {
		http.Error(w, `{"error":"no such sandbox"}`, http.StatusNotFound)
		return
	}
	buf, err := packTree(dir)
	if err != nil {
		http.Error(w, `{"error":"pack"}`, http.StatusInternalServerError)
		return
	}
	_, _ = io.Copy(w, buf)
}

func (f *fakeSandboxd) destroy(w http.ResponseWriter, r *http.Request) {
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

func (f *fakeSandboxd) counts() (created, destroyed, open int) {
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

// world is one build's surroundings: a bare repository standing in for
// GitHub's git side, the fake GitHub API, the fake sandboxd and a runner.
type world struct {
	t      *testing.T
	bare   string
	gh     *githubtest.Server
	sb     *fakeSandboxd
	app    *github.App
	runner *Runner

	mu        sync.Mutex
	decisions []recorded
	events    []string
	artifacts map[string][]byte
	failRec   bool
}

type recorded struct {
	Req PermissionRequest
	D   Decision
}

func runGit(t *testing.T, dir string, args ...string) string {
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

// newWorld seeds a repository with orbit's shape: code, a workflow, a
// deploy directory.
func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, artifacts: map[string][]byte{}}
	root := t.TempDir()
	w.bare = filepath.Join(root, "orbit.git")
	runGit(t, root, "init", "--quiet", "--bare", w.bare)
	seed := filepath.Join(root, "seed")
	runGit(t, root, "init", "--quiet", seed)
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
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "--quiet", "-m", "seed")
	runGit(t, seed, "push", "--quiet", w.bare, "HEAD:refs/heads/main")
	runGit(t, w.bare, "symbolic-ref", "HEAD", "refs/heads/main")

	w.gh = githubtest.New(t)
	w.gh.Mu.Lock()
	w.gh.PR = nil
	w.gh.BranchHead = func(ref string) string {
		out, err := exec.Command("git", "-C", w.bare, "rev-parse", "refs/heads/"+ref).Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	w.gh.Mu.Unlock()
	w.app = &github.App{ID: githubtest.AppID, Key: githubtest.Key, BaseURL: w.gh.URL(), HTTP: w.gh.Srv.Client(), Now: time.Now}
	w.sb = newFakeSandboxd(t)
	w.runner = &Runner{WorkRoot: t.TempDir(), CloneURL: func(string) string { return w.bare }}
	return w
}

func (w *world) deps() Deps {
	return Deps{
		App:     w.app,
		Sandbox: w.sb.client(w.t),
		Emit: func(kind string, payload []byte) {
			w.mu.Lock()
			w.events = append(w.events, kind+" "+string(payload))
			w.mu.Unlock()
		},
		Artifact: func(dir, name, mt string, content []byte) error {
			w.mu.Lock()
			w.artifacts[dir+"/"+name] = content
			w.mu.Unlock()
			return nil
		},
		Record: func(r PermissionRequest, d Decision) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			if w.failRec {
				return fmt.Errorf("database is down")
			}
			w.decisions = append(w.decisions, recorded{r, d})
			return nil
		},
	}
}

const testTicket = "3f2a9c1e-0000-4000-8000-000000000001"

const testCredential = "sk-ant-oat01-test-credential-value-0123456789"

func (w *world) job(runID string, steps ...map[string]any) Job {
	s, _ := json.Marshal(map[string]any{"steps": steps})
	return Job{
		RunID: runID, Attempt: 1, TicketID: testTicket, Title: "Add a hello file", Repo: "kensac/orbit",
		Plan: "Add hello.txt.\n\nFAKE-SCRIPT: " + string(s), PlanVersion: 2,
		Ticket:  json.RawMessage(`{"title":"Add a hello file","body":"please"}`),
		Purpose: "build",
		Policy:  orbitRepo,
		Model:   "m", MaxTurns: 30, Timeout: time.Minute,
		Credential: testCredential,
		Author:     Identity{Name: "orbit-kensac[bot]", Email: "351241810+orbit-kensac[bot]@users.noreply.github.com"},
	}
}

func (w *world) remoteBranch() string {
	out, err := exec.Command("git", "-C", w.bare, "rev-parse", "--verify", "--quiet", "refs/heads/orbit/3f2a9c1e").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (w *world) hasEvent(prefix string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range w.events {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

var _ net.Conn // keep net for the hijacked connection type
