package sandboxd

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/credproxy"
	"github.com/kensac/orbit/internal/sandbox"
)

// fakeDocker is a Docker Engine API on a unix socket, enough of one to run
// sandboxd's whole flow, recording every request it is sent.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer
	volumes    map[string]map[string]string
	created    []containerCreate
	execs      map[string]execCreate
	paths      []string
	// tamper changes the inspect sandboxd's create verifies.
	tamper func(*containerState)
	// extra is added to every readback.
	extra []entry
	// names maps container names to ids (a create with ?name=).
	names map[string]string
	// createQueries are the query strings of every create.
	createQueries []string
	// ttys are the running fake TTY processes by pid; resizes are what
	// each exec was resized to.
	ttys    map[int]net.Conn
	resizes map[string][]string
	nextPid int
	// sessions are the tmux sessions has-session reports alive.
	sessions map[string]bool
	exits    map[string]int
	// home, when set, is the HOME a box's fixed scripts (sh -c with a
	// $0 of runScripts) really run against on this machine; bin, when set,
	// is put first on their PATH (fake curl, claude).
	home, bin string
}

// runScripts are the $0 names of a box's fixed scripts the fake runs.
var runScripts = map[string]bool{"orbit-account": true}

type fakeContainer struct {
	spec    containerCreate
	labels  map[string]string
	running bool
	// health is the healthcheck status; "" is a container without one.
	health string
	files  []entry
}

func newID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (f *fakeDocker) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	p := "/" + apiVersion
	mux.HandleFunc("GET "+p+"/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(dockerInfo{SecurityOptions: []string{"name=seccomp,profile=builtin"}})
	})
	mux.HandleFunc("GET "+p+"/networks/{name}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(networkState{Name: r.PathValue("name"), Internal: r.PathValue("name") == "orbit-sandbox" || r.PathValue("name") == "orbit-workstation"})
	})
	mux.HandleFunc("GET "+p+"/images/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, testImage+"/json") {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("GET "+p+"/containers/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []listedContainer{}
		for id, c := range f.containers {
			if c.labels[LabelManaged] == "1" {
				out = append(out, listedContainer{ID: id, Labels: c.labels})
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST "+p+"/containers/create", func(w http.ResponseWriter, r *http.Request) {
		var spec containerCreate
		if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
			t.Error(err)
		}
		id := newID()
		f.mu.Lock()
		f.created = append(f.created, spec)
		f.createQueries = append(f.createQueries, r.URL.RawQuery)
		if n := r.URL.Query().Get("name"); n != "" {
			if f.names == nil {
				f.names = map[string]string{}
			}
			if _, taken := f.names[n]; taken {
				f.mu.Unlock()
				w.WriteHeader(http.StatusConflict)
				return
			}
			f.names[n] = id
		}
		f.containers[id] = &fakeContainer{spec: spec, labels: spec.Labels}
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
	})
	get := func(w http.ResponseWriter, id string) *fakeContainer {
		f.mu.Lock()
		defer f.mu.Unlock()
		if n, ok := f.names[id]; ok {
			id = n
		}
		c := f.containers[id]
		if c == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "No such container: " + id})
		}
		return c
	}
	mux.HandleFunc("GET "+p+"/containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		c := get(w, r.PathValue("id"))
		if c == nil {
			return
		}
		var st containerState
		st.ID = r.PathValue("id")
		f.mu.Lock()
		if n, ok := f.names[st.ID]; ok {
			st.ID = n
		}
		f.mu.Unlock()
		st.Config.User = c.spec.User
		st.Config.Image = c.spec.Image
		st.Created = "2026-11-02T00:00:00Z"
		st.HostConfig.Binds = c.spec.HostConfig.Binds
		st.Config.Labels = c.labels
		h := c.spec.HostConfig
		st.HostConfig.Privileged = h.Privileged
		st.HostConfig.ReadonlyRootfs = h.ReadonlyRootfs
		st.HostConfig.CapAdd = h.CapAdd
		st.HostConfig.CapDrop = h.CapDrop
		st.HostConfig.SecurityOpt = h.SecurityOpt
		st.HostConfig.NetworkMode = h.NetworkMode
		st.HostConfig.UTSMode = h.UTSMode
		st.HostConfig.CgroupnsMode = h.CgroupnsMode
		st.HostConfig.IpcMode = h.IpcMode
		st.HostConfig.Memory = h.Memory
		st.HostConfig.MemorySwap = h.MemorySwap
		st.HostConfig.NanoCpus = h.NanoCpus
		st.HostConfig.Tmpfs = h.Tmpfs
		st.HostConfig.Devices = h.Devices
		st.HostConfig.PublishAllPorts = h.PublishAllPorts
		st.HostConfig.PortBindings = h.PortBindings
		st.HostConfig.ExtraHosts = h.ExtraHosts
		st.HostConfig.Runtime = h.Runtime
		pl := h.PidsLimit
		st.HostConfig.PidsLimit = &pl
		for _, m := range h.Mounts {
			st.Mounts = append(st.Mounts, mountState{Type: m.Type, Name: m.Source, Destination: m.Target})
		}
		for destination := range h.Tmpfs {
			st.Mounts = append(st.Mounts, mountState{Type: "tmpfs", Destination: destination})
		}
		st.State.Running = c.running
		if c.health != "" {
			st.State.Health = &struct {
				Status string `json:"Status"`
			}{c.health}
		}
		if c.running && h.NetworkMode != "none" {
			st.NetworkSettings.Networks = map[string]struct {
				IPAddress string `json:"IPAddress"`
			}{h.NetworkMode: {IPAddress: "10.250.251.7"}}
		}
		if f.tamper != nil {
			f.tamper(&st)
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("PUT "+p+"/containers/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		c := get(w, r.PathValue("id"))
		if c == nil {
			return
		}
		if r.URL.Query().Get("path") != "/work" {
			t.Errorf("archive path %q", r.URL.Query().Get("path"))
		}
		var es []entry
		tr := tar.NewReader(r.Body)
		for {
			th, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			b, _ := io.ReadAll(tr)
			es = append(es, entry{*th, string(b)})
		}
		f.mu.Lock()
		c.files = es
		f.mu.Unlock()
	})
	mux.HandleFunc("GET "+p+"/containers/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		c := get(w, r.PathValue("id"))
		if c == nil {
			return
		}
		es := []entry{{h: tar.Header{Typeflag: tar.TypeDir, Name: "work/", Mode: 0o755}}}
		for _, e := range c.files {
			if e.h.Name == "./" {
				continue
			}
			e.h.Name = "work/" + e.h.Name
			es = append(es, e)
		}
		es = append(es, f.extra...)
		_, _ = io.Copy(w, mkTar(t, es...))
	})
	mux.HandleFunc("POST "+p+"/containers/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		if c := get(w, r.PathValue("id")); c != nil {
			f.mu.Lock()
			c.running = true
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("POST "+p+"/containers/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		if c := get(w, r.PathValue("id")); c != nil {
			f.mu.Lock()
			c.running = false
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("POST "+p+"/exec/{id}/resize", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if f.resizes == nil {
			f.resizes = map[string][]string{}
		}
		f.resizes[r.PathValue("id")] = append(f.resizes[r.PathValue("id")], r.URL.Query().Get("w")+"x"+r.URL.Query().Get("h"))
		f.mu.Unlock()
	})
	mux.HandleFunc("DELETE "+p+"/containers/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") != "true" || r.URL.Query().Get("v") != "true" {
			t.Errorf("remove without force and v: %s", r.URL.RawQuery)
		}
		if c := get(w, r.PathValue("id")); c != nil {
			f.mu.Lock()
			for n, id := range f.names {
				if id == r.PathValue("id") {
					delete(f.names, n)
				}
			}
			delete(f.containers, r.PathValue("id"))
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("GET "+p+"/volumes", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		type vol struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
		}
		var out struct {
			Volumes []vol `json:"Volumes"`
		}
		for n, l := range f.volumes {
			// Like a daemon that ignored the filter: sandboxd must check.
			out.Volumes = append(out.Volumes, vol{n, l})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("DELETE "+p+"/volumes/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		delete(f.volumes, r.PathValue("name"))
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST "+p+"/containers/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		if c := get(w, r.PathValue("id")); c == nil {
			return
		}
		var e execCreate
		_ = json.NewDecoder(r.Body).Decode(&e)
		id := newID()
		f.mu.Lock()
		f.execs[id] = e
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"Id": id})
	})
	// The fake process echoes its stdin to stdout once stdin closes, says
	// "bye" on stderr, and exits 7.
	mux.HandleFunc("POST "+p+"/exec/{id}/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "tcp" {
			t.Error("exec start without upgrade")
		}
		var start struct {
			Tty         bool  `json:"Tty"`
			ConsoleSize []int `json:"ConsoleSize"`
		}
		_ = json.NewDecoder(r.Body).Decode(&start)
		f.mu.Lock()
		e := f.execs[r.PathValue("id")]
		f.mu.Unlock()
		if start.Tty != e.Tty {
			t.Errorf("exec started with Tty %v, created with %v", start.Tty, e.Tty)
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = buf.Flush()
		if e.Tty {
			f.fakeTTY(r.PathValue("id"), conn, buf.Reader)
			return
		}
		if len(e.Cmd) > 0 && (e.Cmd[0] == "tmux" || e.Cmd[0] == "kill") {
			f.fakeCommand(r.PathValue("id"), e.Cmd)
			return
		}
		frame := func(stream byte, p []byte) {
			var h [8]byte
			h[0] = stream
			binary.BigEndian.PutUint32(h[4:], uint32(len(p)))
			_, _ = conn.Write(h[:])
			_, _ = conn.Write(p)
		}
		if f.home != "" && len(e.Cmd) > 3 && e.Cmd[0] == "sh" && runScripts[e.Cmd[3]] {
			var stdin []byte
			if e.AttachStdin {
				stdin, _ = io.ReadAll(buf.Reader)
			}
			out, errb, code := f.runScript(t, e, stdin)
			frame(1, out)
			frame(2, errb)
			f.mu.Lock()
			f.exits[r.PathValue("id")] = code
			f.mu.Unlock()
			return
		}
		in, _ := io.ReadAll(buf.Reader)
		frame(1, in)
		frame(2, []byte("bye"))
	})
	mux.HandleFunc("GET "+p+"/exec/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		code, ok := f.exits[r.PathValue("id")]
		f.mu.Unlock()
		if !ok {
			code = 7
		}
		_ = json.NewEncoder(w).Encode(execState{Running: false, ExitCode: code})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
}

// runScript runs a box's fixed script here, as the box would: HOME is the
// fake's home and the exec's environment is added.
func (f *fakeDocker) runScript(t *testing.T, e execCreate, stdin []byte) (stdout, stderr []byte, code int) {
	cmd := exec.Command(e.Cmd[0], e.Cmd[1:]...)
	path := os.Getenv("PATH")
	if f.bin != "" {
		path = f.bin + string(os.PathListSeparator) + path
	}
	cmd.Env = append([]string{"HOME=" + f.home, "PATH=" + path}, e.Env...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		code = ee.ExitCode()
	case err != nil:
		t.Errorf("script %v: %v", e.Cmd[3:], err)
		code = -1
	}
	return out.Bytes(), errb.Bytes(), code
}

// socketDir is short: a unix socket path is limited to about 100 bytes, and
// macOS's t.TempDir is long.
func socketDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "sbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func serveUnix(t *testing.T, path string, h http.Handler) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
}

type harness struct {
	fd     *fakeDocker
	srv    *Server
	client *sandbox.Client
	sock   string
}

func newHarness(t *testing.T) *harness {
	dir := socketDir(t)
	fd := &fakeDocker{containers: map[string]*fakeContainer{}, volumes: map[string]map[string]string{}, execs: map[string]execCreate{},
		names: map[string]string{}, ttys: map[int]net.Conn{}, sessions: map[string]bool{}, exits: map[string]int{}, nextPid: 41}
	serveUnix(t, filepath.Join(dir, "docker.sock"), fd.handler(t))
	cfg := testConfig()
	cfg.Docker = filepath.Join(dir, "docker.sock")
	cfg.Listen = "unix:" + filepath.Join(dir, "sandboxd.sock")
	s := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	serveUnix(t, filepath.Join(dir, "sandboxd.sock"), s.Handler())
	c, err := sandbox.NewClient(cfg.Listen, cfg.Token)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{fd: fd, srv: s, client: c, sock: filepath.Join(dir, "sandboxd.sock")}
}

func workTar(t *testing.T) *bytes.Buffer {
	return mkTar(t,
		entry{h: tar.Header{Typeflag: tar.TypeDir, Name: "src/", Mode: 0o755}},
		entry{h: tar.Header{Typeflag: tar.TypeReg, Name: "src/a.go", Mode: 0o644}, body: "package a"},
	)
}

func TestServerFlow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.srv.Check(ctx); err != nil {
		t.Fatal(err)
	}
	cr, err := h.client.Create(ctx, "run-1", workTar(t))
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(cr.ExpiresAt) < Lifetime-time.Minute {
		t.Fatalf("expires %v", cr.ExpiresAt)
	}
	h.fd.mu.Lock()
	created := h.fd.created[0]
	files := h.fd.containers[cr.ID].files
	running := h.fd.containers[cr.ID].running
	h.fd.mu.Unlock()
	want := containerSpec(testConfig(), "run-1", time.Now())
	// A copy: the fake container shares the created spec's label map.
	created.Labels = maps.Clone(created.Labels)
	created.Labels[LabelExpires], want.Labels[LabelExpires] = "", ""
	if gb, wb := mustJSON(t, created), mustJSON(t, want); gb != wb {
		t.Fatalf("docker received a different spec:\n%s\nwant\n%s", gb, wb)
	}
	if !running || len(files) != 3 || files[0].h.Name != "./" || files[2].body != "package a" || files[2].h.Uid != UID {
		t.Fatalf("running %v, files %+v", running, files)
	}

	var stdout, stderr bytes.Buffer
	code, err := h.client.Attach(ctx, cr.ID, sandbox.AttachRequest{
		Argv: []string{"cat"},
		Env:  map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"},
	}, strings.NewReader("ping"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 || stdout.String() != "ping" || stderr.String() != "bye" {
		t.Fatalf("attach: code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
	// The process gets a token in the credential's place, sealed for this sandbox's address and expiry.
	h.fd.mu.Lock()
	for _, e := range h.fd.execs {
		if e.User != "10001:10001" || e.Privileged || e.WorkingDir != "/work" || len(e.Env) != 1 || !strings.HasPrefix(e.Env[0], "CLAUDE_CODE_OAUTH_TOKEN=orbit-cp1.") {
			t.Fatalf("exec spec %+v", e)
		}
		sealer, _ := credproxy.NewSealer(testConfig().CredProxyKey)
		g, err := sealer.Open(strings.TrimPrefix(e.Env[0], "CLAUDE_CODE_OAUTH_TOKEN="))
		if err != nil || g.Credential != "tok" || g.Kind != credproxy.KindOAuth || g.Addr.String() != "10.250.251.7" ||
			!g.Expires.Equal(cr.ExpiresAt) || g.Sandbox != cr.ID || g.RunID != "run-1" {
			t.Fatalf("grant %+v, %v", g, err)
		}
	}
	h.fd.mu.Unlock()

	rc, err := h.client.Archive(ctx, cr.ID)
	if err != nil {
		t.Fatal(err)
	}
	es := readTar(t, rc)
	rc.Close()
	if len(es) != 2 || es[0].h.Name != "src/" || es[1].h.Name != "src/a.go" || es[1].body != "package a" {
		t.Fatalf("readback %+v", es)
	}

	if err := h.client.Destroy(ctx, cr.ID); err != nil {
		t.Fatal(err)
	}
	if len(h.fd.containers) != 0 {
		t.Fatal("container not removed")
	}
	if err := h.client.Destroy(ctx, cr.ID); err != nil {
		t.Fatalf("second destroy: %v", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestServerRefusesWithoutToken(t *testing.T) {
	h := newHarness(t)
	for _, tok := range []string{"", "Bearer wrong", "Bearer " + strings.Repeat("t", 39), strings.Repeat("t", 40)} {
		conn, err := net.Dial("unix", h.sock)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodDelete, "http://x"+sandbox.PathSandboxes+"/"+strings.Repeat("a", 64), nil)
		if tok != "" {
			req.Header.Set("Authorization", tok)
		}
		_ = req.Write(conn)
		resp, err := http.ReadResponse(bufioReader(conn), req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		conn.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: %d", tok, resp.StatusCode)
		}
	}
	if len(h.fd.paths) != 0 {
		t.Fatalf("docker was called: %v", h.fd.paths)
	}
}

// TestServerOnlyTouchesItsOwnContainers: a container sandboxd did not make,
// addressed by its full id, is "no such sandbox" for every operation, and
// docker is never asked to change it.
func TestServerOnlyTouchesItsOwnContainers(t *testing.T) {
	h := newHarness(t)
	other := newID()
	h.fd.containers[other] = &fakeContainer{labels: map[string]string{"com.docker.compose.service": "orbit"}, running: true}
	ctx := context.Background()
	var se *sandbox.Error
	_, err := h.client.Archive(ctx, other)
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Fatalf("archive of another container: %v", err)
	}
	_, err = h.client.Attach(ctx, other, sandbox.AttachRequest{Argv: []string{"sh"}}, nil, nil, nil)
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Fatalf("attach to another container: %v", err)
	}
	_ = h.client.Destroy(ctx, other)
	if h.fd.containers[other] == nil {
		t.Fatal("another container was removed")
	}
	for _, p := range h.fd.paths {
		if !strings.HasSuffix(p, "/json") {
			t.Errorf("docker called with %s", p)
		}
	}
}

func TestServerCreateLimitsAndFailures(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for i := range MaxSandboxes {
		if _, err := h.client.Create(ctx, "run-"+string(rune('a'+i)), workTar(t)); err != nil {
			t.Fatal(err)
		}
	}
	var se *sandbox.Error
	if _, err := h.client.Create(ctx, "run-z", workTar(t)); !errors.As(err, &se) || se.Status != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %v", err)
	}

	h = newHarness(t)
	bad := mkTar(t, entry{h: tar.Header{Typeflag: tar.TypeChar, Name: "dev/null"}})
	if _, err := h.client.Create(ctx, "run-1", bad); !errors.As(err, &se) || se.Status != http.StatusBadRequest {
		t.Fatalf("device in archive: %v", err)
	}
	if len(h.fd.containers) != 0 {
		t.Fatal("a refused create left its container")
	}

	h.fd.tamper = func(c *containerState) { c.HostConfig.Privileged = true }
	if _, err := h.client.Create(ctx, "run-2", workTar(t)); !errors.As(err, &se) || se.Status != http.StatusInternalServerError {
		t.Fatalf("tampered container: %v", err)
	}
	if len(h.fd.containers) != 0 {
		t.Fatal("a container the daemon altered was kept")
	}
	if _, err := h.client.Create(ctx, "bad/run", workTar(t)); err == nil {
		t.Fatal("bad run id accepted")
	}
}

func TestServerAttachRefusesOptions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cr, err := h.client.Create(ctx, "run-1", workTar(t))
	if err != nil {
		t.Fatal(err)
	}
	// A raw request with a field the protocol does not have.
	for _, body := range []string{
		`{"argv":["sh"],"user":"0:0"}`,
		`{"argv":["sh"],"privileged":true}`,
		`{"argv":["sh"],"env":{"PATH":"/x"}}`,
		`{"argv":[]}`,
	} {
		conn, err := net.Dial("unix", h.sock)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest(http.MethodPost, "http://x"+sandbox.PathSandboxes+"/"+cr.ID+"/attach", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testConfig().Token)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", sandbox.AttachUpgrade)
		_ = req.Write(conn)
		resp, err := http.ReadResponse(bufioReader(conn), req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		conn.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", body, resp.StatusCode)
		}
	}
	if len(h.fd.execs) != 0 {
		t.Fatal("an exec was created for a refused request")
	}
}

func TestServerReadbackRefusesSpecialFiles(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cr, err := h.client.Create(ctx, "run-1", workTar(t))
	if err != nil {
		t.Fatal(err)
	}
	h.fd.extra = []entry{{h: tar.Header{Typeflag: tar.TypeFifo, Name: "work/pipe"}}}
	var se *sandbox.Error
	if _, err := h.client.Archive(ctx, cr.ID); !errors.As(err, &se) || se.Status != http.StatusUnprocessableEntity || !strings.Contains(se.Message, "work/pipe") {
		t.Fatalf("fifo readback: %v", err)
	}
}

func TestReap(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.srv.now = func() time.Time { return now }
	exp := func(d time.Duration) string { return strings.TrimSpace(mustJSON(t, now.Add(d).Unix())) }
	fresh, expired, grace, unlabeled := newID(), newID(), newID(), newID()
	h.fd.containers[fresh] = &fakeContainer{labels: map[string]string{LabelManaged: "1", LabelExpires: exp(time.Hour)}}
	h.fd.containers[grace] = &fakeContainer{labels: map[string]string{LabelManaged: "1", LabelExpires: exp(-Grace / 2)}}
	h.fd.containers[expired] = &fakeContainer{labels: map[string]string{LabelManaged: "1", LabelExpires: exp(-Grace - time.Minute)}}
	h.fd.containers[unlabeled] = &fakeContainer{labels: map[string]string{LabelExpires: exp(-48 * time.Hour)}}
	h.fd.volumes["ours"] = map[string]string{LabelManaged: "1"}
	h.fd.volumes["root_plaid_db_data_v18"] = map[string]string{}
	h.srv.Reap(context.Background())
	if h.fd.containers[expired] != nil {
		t.Error("expired sandbox kept")
	}
	if h.fd.containers[fresh] == nil || h.fd.containers[grace] == nil {
		t.Error("a live or in-grace sandbox was reaped")
	}
	if h.fd.containers[unlabeled] == nil {
		t.Error("a container without the label was reaped")
	}
	if _, ok := h.fd.volumes["ours"]; ok {
		t.Error("dangling sandbox volume kept")
	}
	if _, ok := h.fd.volumes["root_plaid_db_data_v18"]; !ok {
		t.Fatal("a volume without the label was removed")
	}
}

func TestCheckRefusesOpenNetwork(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Network = "bridge-ish"
	if err := h.srv.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "not internal") {
		t.Fatalf("check on a non-internal network: %v", err)
	}
	h.srv.cfg.Network, h.srv.cfg.WorkstationProxy, h.srv.cfg.WorkstationNetwork = "orbit-sandbox", "orbit-egress-open:3128", "open-ish"
	if err := h.srv.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "open-ish is not internal") {
		t.Fatalf("check on a non-internal workstation network: %v", err)
	}
}

func bufioReader(c net.Conn) *bufio.Reader { return bufio.NewReader(c) }

func TestProfiles(t *testing.T) {
	s := &Server{cfg: Config{Proxy: "orbit-egress:3128"}}
	if _, err := s.profile("workstation"); err == nil {
		t.Fatal("an unconfigured workstation profile was accepted")
	}
	s.cfg.Network, s.cfg.WorkstationProxy, s.cfg.WorkstationNetwork = "orbit-sandbox", "orbit-egress-open:3128", "orbit-workstation"
	for name, want := range map[string][2]string{
		"":            {"orbit-egress:3128", "orbit-sandbox"},
		"build":       {"orbit-egress:3128", "orbit-sandbox"},
		"workstation": {"orbit-egress-open:3128", "orbit-workstation"},
	} {
		cfg, err := s.profile(name)
		if err != nil || cfg.Proxy != want[0] || cfg.Network != want[1] {
			t.Errorf("%q: %q on %q, %v, want %v", name, cfg.Proxy, cfg.Network, err, want)
		}
	}
	s.cfg.CredProxy, s.cfg.WorkstationCredProxy = "10.251.0.3:3129", "10.251.1.3:3129"
	if b, _ := s.profile("build"); b.CredProxy != "10.251.0.3:3129" {
		t.Errorf("build credproxy %q", b.CredProxy)
	}
	if w, _ := s.profile("workstation"); w.CredProxy != "10.251.1.3:3129" {
		t.Errorf("workstation credproxy %q", w.CredProxy)
	}
	if _, err := s.profile("root"); err == nil {
		t.Fatal("an unknown profile was accepted")
	}
}

// TestWorkstationOwnNetwork: a workstation sandbox joins its own network with the open proxy, never the builds' network, so no build shares a network with that proxy.
func TestWorkstationOwnNetwork(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.WorkstationProxy, h.srv.cfg.WorkstationNetwork = "orbit-egress-open:3128", "orbit-workstation"
	ctx := context.Background()
	if err := h.srv.Check(ctx); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{sandbox.ProfileBuild, sandbox.ProfileWorkstation} {
		if _, err := h.client.CreateProfile(ctx, "run-"+profile, profile, workTar(t)); err != nil {
			t.Fatalf("%s: %v", profile, err)
		}
	}
	h.fd.mu.Lock()
	defer h.fd.mu.Unlock()
	got := map[string]string{}
	for _, c := range h.fd.created {
		proxy := ""
		for _, e := range c.Env {
			if v, ok := strings.CutPrefix(e, "HTTPS_PROXY="); ok {
				proxy = v
			}
		}
		got[c.Labels[LabelRunID]] = c.HostConfig.NetworkMode + " " + proxy
	}
	if got["run-build"] != "orbit-sandbox http://orbit-egress:3128" || got["run-workstation"] != "orbit-workstation http://orbit-egress-open:3128" {
		t.Fatalf("networks and proxies: %v", got)
	}
}
