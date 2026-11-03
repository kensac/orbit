package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/auth"
	"github.com/kensac/orbit/internal/auth/authtest"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

const (
	portfolio = "https://kanishksachdev.com"
	sbToken   = "sandbox-token-sandbox-token-sandbox-token"
)

// fakeSandboxd speaks sandboxd's box protocol: boxes start on ensure, a
// TTY echoes its input and exits 0 on "exit" (ending its session).
type fakeSandboxd struct {
	mu       sync.Mutex
	running  map[string]bool
	sessions map[string]bool
	ttys     []sandbox.TTYRequest
	resizes  []sandbox.Resize
	killed   []string
	hangups  int
	// mounts are each box's account slots; recreates counts recreates;
	// prepared lists prepared slots; logins are what an account's status
	// read answers.
	mounts    map[string][]string
	recreates int
	prepared  []string
	logins    map[string]sandbox.AccountStatus
	configs   map[string]sandbox.ClaudeConfigStatus
	bundles   [][]byte
}

func (f *fakeSandboxd) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	status := func(w http.ResponseWriter, name string) {
		st := sandbox.BoxStatus{Name: name, State: sandbox.BoxNone, Host: "hetzner", Arch: "amd64"}
		if f.running[name] {
			st.State, st.ID, st.Image = sandbox.BoxRunning, strings.Repeat("b", 64), "ghcr.io/kensac/orbit-workstation@sha256:"+strings.Repeat("c", 64)
		}
		st.Accounts = f.mounts[name]
		_ = json.NewEncoder(w).Encode(st)
	}
	union := func(name string, r *http.Request) {
		var req sandbox.EnsureRequest
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Validate() != nil {
				t.Errorf("ensure body %+v %v", req, err)
			}
		}
		for _, a := range req.Accounts {
			if !slices.Contains(f.mounts[name], a) {
				f.mounts[name] = append(f.mounts[name], a)
			}
		}
		slices.Sort(f.mounts[name])
	}
	mux.HandleFunc("GET /v1/boxes/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		status(w, r.PathValue("name"))
	})
	mux.HandleFunc("POST /v1/boxes/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.running[r.PathValue("name")] {
			union(r.PathValue("name"), r)
		}
		f.running[r.PathValue("name")] = true
		status(w, r.PathValue("name"))
	})
	mux.HandleFunc("POST /v1/boxes/{name}/recreate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		union(r.PathValue("name"), r)
		f.recreates++
		f.sessions = map[string]bool{}
		f.running[r.PathValue("name")] = true
		status(w, r.PathValue("name"))
	})
	mux.HandleFunc("POST /v1/boxes/{name}/accounts/{slot}/prepare", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !slices.Contains(f.mounts[r.PathValue("name")], r.PathValue("slot")) {
			w.WriteHeader(http.StatusConflict)
			return
		}
		f.prepared = append(f.prepared, r.PathValue("slot"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/boxes/{name}/accounts/{slot}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(f.logins[r.PathValue("slot")])
	})
	mux.HandleFunc("GET /v1/boxes/{name}/accounts/{slot}/claude-config", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		st, ok := f.configs[r.PathValue("slot")]
		if !ok {
			st = sandbox.ClaudeConfigStatus{Digest: sandbox.ClaudeConfigDigest(nil), Files: []sandbox.ClaudeConfigFile{}}
		}
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("PUT /v1/boxes/{name}/accounts/{slot}/claude-config", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		slot := r.PathValue("slot")
		st, ok := f.configs[slot]
		if !ok {
			st = sandbox.ClaudeConfigStatus{Digest: sandbox.ClaudeConfigDigest(nil), Files: []sandbox.ClaudeConfigFile{}}
		}
		if r.Header.Get("If-Match") != st.Digest {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "config changed"})
			return
		}
		b, _ := io.ReadAll(r.Body)
		h := sha256.Sum256(b)
		if hex.EncodeToString(h[:]) != r.Header.Get("X-Orbit-Bundle-SHA256") {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, incoming, err := sandbox.ParseClaudeConfigTar(b)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		byPath := map[string]sandbox.ClaudeConfigFile{}
		for _, file := range st.Files {
			byPath[file.Path] = file
		}
		for _, file := range incoming.Files {
			byPath[file.Path] = file
		}
		st.Files = st.Files[:0]
		for _, file := range byPath {
			st.Files = append(st.Files, file)
		}
		slices.SortFunc(st.Files, func(a, b sandbox.ClaudeConfigFile) int { return strings.Compare(a.Path, b.Path) })
		st.Digest = sandbox.ClaudeConfigDigest(st.Files)
		f.configs[slot] = st
		f.bundles = append(f.bundles, bytes.Clone(b))
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("GET /v1/boxes/{name}/sessions/{s}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]bool{"exists": f.sessions[r.PathValue("s")]})
	})
	mux.HandleFunc("POST /v1/boxes/{name}/sessions/{s}/kill", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.sessions, r.PathValue("s"))
		f.killed = append(f.killed, r.PathValue("s"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/boxes/{name}/tty", func(w http.ResponseWriter, r *http.Request) {
		var tr sandbox.TTYRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&tr); err != nil || tr.Validate() != nil || r.Header.Get("Upgrade") != sandbox.TTYUpgrade {
			t.Errorf("tty request %+v %v", tr, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.ttys = append(f.ttys, tr)
		f.sessions[tr.Session] = true
		f.mu.Unlock()
		conn, buf, _ := w.(http.Hijacker).Hijack()
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + sandbox.TTYUpgrade + "\r\n\r\n")
		_ = buf.Flush()
		for {
			typ, p, err := sandbox.ReadFrame(buf.Reader)
			if err != nil {
				f.mu.Lock()
				f.hangups++
				f.mu.Unlock()
				return
			}
			switch typ {
			case sandbox.FrameResize:
				var rz sandbox.Resize
				_ = json.Unmarshal(p, &rz)
				f.mu.Lock()
				f.resizes = append(f.resizes, rz)
				f.mu.Unlock()
			case sandbox.FrameStdin:
				_ = sandbox.WriteFrame(conn, sandbox.FrameStdout, p)
				if bytes.Contains(p, []byte("exit")) {
					f.mu.Lock()
					delete(f.sessions, tr.Session)
					f.mu.Unlock()
					b, _ := json.Marshal(sandbox.Exit{ExitCode: 0})
					_ = sandbox.WriteFrame(conn, sandbox.FrameExit, b)
					return
				}
			}
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+sbToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

type boxEnv struct {
	*env
	fake  *fakeSandboxd
	drain *engine.Drain
}

func newBoxEnv(t *testing.T) *boxEnv {
	t.Helper()
	fake := &fakeSandboxd{running: map[string]bool{}, sessions: map[string]bool{}, mounts: map[string][]string{}, logins: map[string]sandbox.AccountStatus{}, configs: map[string]sandbox.ClaudeConfigStatus{}}
	sb := httptest.NewServer(fake.handler(t))
	t.Cleanup(sb.Close)
	addr := strings.TrimPrefix(sb.URL, "http://")

	s := testdb.New(t)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(`
public_url: https://orbit.kanishksachdev.com
defaults: {runner: r, channels: [c]}
runners: {r: {kind: claude-code}}
channels: {c: {kind: log}}
sandbox: {sandboxd: "`+addr+`"}
`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "a.yaml"), []byte("name: a\ncharter: x\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "box.yaml"), []byte("name: box\ncharter: x\nchat: true\nsandbox: workstation\n"), 0o644)
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &engine.Engine{Store: s, ConfigDir: dir, Secret: []byte(token), Client: http.DefaultClient, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Env: func(k string) (string, bool) {
			return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sub-token"}[k], k == "CLAUDE_CODE_OAUTH_TOKEN"
		}}
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	is := authtest.NewIssuer(t)
	ver, err := auth.New(auth.Config{Issuers: []string{is.URL}, ClientIDs: []string{clientID}, Group: group})
	if err != nil {
		t.Fatal(err)
	}
	client, err := sandbox.NewClient(addr, sbToken)
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	drain := engine.NewDrain()
	a := &api.Server{Store: s, Engine: e, Token: token, Verifier: ver, Wake: func() {}, Stop: stop, Drain: drain,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), TerminalPing: 50 * time.Millisecond,
		Boxes: func() (*sandbox.Client, error) { return client, nil }}
	pub := httptest.NewUnstartedServer(a.Handler())
	// Ordinary requests use the same allowance as the conversation tests so
	// database-backed handlers can finish under -race on a loaded CI runner.
	pub.Config.WriteTimeout = testWriteTimeout
	pub.Start()
	internal := httptest.NewServer(a.InternalHandler())
	t.Cleanup(func() {
		close(stop)
		pub.Close()
		internal.Close()
	})
	return &boxEnv{env: &env{srv: pub, internal: internal, s: s, e: e, is: is}, fake: fake, drain: drain}
}

func (v *boxEnv) newTerminal(t *testing.T, body string) map[string]any {
	t.Helper()
	code, out := call(t, http.MethodGet, v.srv.URL+"/api/v1/agents/box/workspace", v.person("kanishk"), "")
	ws, _ := out["workspace"].(map[string]any)
	if code != 200 || ws == nil {
		t.Fatalf("workspace: %d %v", code, out)
	}
	code, out = call(t, http.MethodPost, v.srv.URL+"/api/v1/workspaces/"+ws["id"].(string)+"/terminals", v.person("kanishk"), body)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	return out["terminal"].(map[string]any)
}

// ticket mints an attach ticket and returns it with the socket URL.
func (v *boxEnv) ticket(t *testing.T, tid string) (string, string) {
	t.Helper()
	code, out := call(t, http.MethodPost, v.srv.URL+"/api/v1/terminals/"+tid+"/attach", v.person("kanishk"), "")
	if code != 200 {
		t.Fatalf("attach: %d %v", code, out)
	}
	if out["url"] != "wss://orbit.kanishksachdev.com/api/v1/terminals/"+tid+"/ws?ticket="+out["ticket"].(string) || out["protocol"] != api.TerminalProtocol {
		t.Fatalf("attach answer %v", out)
	}
	return out["ticket"].(string), strings.Replace(v.srv.URL, "http://", "ws://", 1) + "/api/v1/terminals/" + tid + "/ws"
}

func dial(t *testing.T, u, ticket, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	protos := []string{api.TerminalProtocol}
	if ticket != "" {
		protos = append(protos, "orbit.ticket."+ticket)
	}
	return websocket.Dial(ctx, u, &websocket.DialOptions{Subprotocols: protos, HTTPHeader: h})
}

func readText(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ != websocket.MessageText {
			t.Fatalf("binary %q where control was expected", b)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if m["type"] != "ping" {
			return m
		}
	}
}

// readBinary returns the next output frame, skipping the server's keepalive
// pings, which may arrive at any point after the ready frame.
func readBinary(t *testing.T, c *websocket.Conn) []byte {
	t.Helper()
	for {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.MessageBinary {
			return b
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil || m["type"] != "ping" {
			t.Fatalf("control %q where output was expected", b)
		}
	}
}

func TestBoxWorkspace(t *testing.T) {
	v := newBoxEnv(t)
	code, out := call(t, http.MethodGet, v.srv.URL+"/api/v1/agents/a/workspace", v.person("kanishk"), "")
	if code != 200 || out["workspace"] != nil {
		t.Fatalf("an agent without a box: %d %v", code, out)
	}
	code, out = call(t, http.MethodGet, v.srv.URL+"/api/v1/agents/box/workspace", v.person("kanishk"), "")
	ws := out["workspace"].(map[string]any)
	caps := ws["capabilities"].(map[string]any)
	box := ws["box"].(map[string]any)
	if code != 200 || ws["scope"] != "agent" || ws["backend"] != "box" || caps["terminals"] != true || box["state"] != "stopped" || box["host"] != "hetzner" {
		t.Fatalf("box workspace: %d %v", code, ws)
	}
	_, again := call(t, http.MethodGet, v.srv.URL+"/api/v1/agents/box/workspace", v.person("kanishk"), "")
	if again["workspace"].(map[string]any)["id"] != ws["id"] {
		t.Fatal("the workspace id is not stable")
	}
	base := v.srv.URL + "/api/v1/workspaces/" + ws["id"].(string) + "/terminals"
	for body, want := range map[string]int{
		`{"preset":"bash","cols":80,"rows":24}`:                              400,
		`{"preset":"shell","cols":0,"rows":24}`:                              400,
		`{"preset":"shell","cols":80,"rows":24,"cwd":"../etc"}`:              400,
		`{"preset":"claude","cols":80,"rows":24,"claude":{"resume":"x"}}`:    400,
		`{"preset":"shell","cols":80,"rows":24,"argv":["sh"]}`:               400,
		`{"preset":"shell","cols":80,"rows":24,"title":"build","cwd":"src"}`: 201,
	} {
		if code, out := call(t, http.MethodPost, base, v.person("kanishk"), body); code != want {
			t.Errorf("%s: %d %v", body, code, out)
		}
	}
	for range 7 {
		if code, _ := call(t, http.MethodPost, base, v.person("kanishk"), `{"preset":"shell","cols":80,"rows":24}`); code != 201 {
			t.Fatal(code)
		}
	}
	if code, out := call(t, http.MethodPost, base, v.person("kanishk"), `{"preset":"shell","cols":80,"rows":24}`); code != 409 || out["check"] != "limit" {
		t.Fatalf("a ninth running terminal: %d %v", code, out)
	}
	_, out = call(t, http.MethodGet, base+"?state=running", v.person("kanishk"), "")
	if n := len(out["terminals"].([]any)); n != 8 {
		t.Fatalf("%d running", n)
	}
	if code, _ := call(t, http.MethodGet, v.srv.URL+"/api/v1/workspaces/"+strings.Repeat("0", 8)+"-0000-0000-0000-000000000000/terminals", v.person("kanishk"), ""); code != 404 {
		t.Fatalf("unknown workspace: %d", code)
	}
}

// TestTerminalSocket is a round trip through a fake sandboxd: the ticket's
// rules, the ready frame, input echoed back as binary, a resize, pings both
// ways past the server's WriteTimeout, and the exit recorded.
func TestTerminalSocket(t *testing.T) {
	v := newBoxEnv(t)
	term := v.newTerminal(t, `{"preset":"shell","cols":80,"rows":24}`)
	tid := term["id"].(string)
	if term["state"] != "running" || term["title"] != "Shell" || term["created_by"] != "kanishk" {
		t.Fatalf("terminal %v", term)
	}

	if code, out := call(t, http.MethodPost, v.srv.URL+"/api/v1/terminals/"+tid+"/attach", v.person("kanishk"), `{"origin":"https://evil.example"}`); code != 400 || out["check"] != "origin" {
		t.Fatalf("a ticket for another origin: %d %v", code, out)
	}
	ticket, u := v.ticket(t, tid)
	if _, resp, err := dial(t, u, ticket, "https://evil.example"); err == nil || resp == nil || resp.StatusCode != 403 {
		t.Fatalf("cross-site socket: %v %v", resp, err)
	}
	if _, resp, err := dial(t, u, "", portfolio); err == nil || resp.StatusCode != 401 {
		t.Fatalf("no ticket: %v %v", resp, err)
	}
	if _, resp, err := dial(t, u, "forged", portfolio); err == nil || resp.StatusCode != 401 {
		t.Fatalf("forged ticket: %v %v", resp, err)
	}
	other := v.newTerminal(t, `{"preset":"shell","cols":80,"rows":24}`)
	otherURL := strings.Replace(u, tid, other["id"].(string), 1)
	if _, resp, err := dial(t, otherURL, ticket, portfolio); err == nil || resp.StatusCode != 401 {
		t.Fatalf("a ticket for another terminal: %v %v", resp, err)
	}

	c, resp, err := dial(t, u, ticket, portfolio)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if c.Subprotocol() != api.TerminalProtocol || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("subprotocol %q", c.Subprotocol())
	}
	if _, resp, err := dial(t, u, ticket, portfolio); err == nil || resp.StatusCode != 401 {
		t.Fatalf("a ticket used twice: %v %v", resp, err)
	}
	ready := readText(t, c)
	if ready["type"] != "ready" || ready["terminal"].(map[string]any)["clients"] != float64(1) {
		t.Fatalf("ready %v", ready)
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":132,"rows":40}`))
	_ = c.Write(ctx, websocket.MessageBinary, []byte("ls\r"))
	if b := readBinary(t, c); string(b) != "ls\r" {
		t.Fatalf("echo %q", b)
	}
	_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"ping"}`))
	if m := readText(t, c); m["type"] != "pong" {
		t.Fatalf("pong %v", m)
	}
	// Past the server's WriteTimeout, the server still pings.
	time.Sleep(testWriteTimeout + 100*time.Millisecond)
	pings := 0
	for pings < 2 {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("the socket died after the WriteTimeout: %v", err)
		}
		if typ == websocket.MessageText && strings.Contains(string(b), `"ping"`) {
			pings++
		}
	}
	_ = c.Write(ctx, websocket.MessageBinary, []byte("exit\r"))
	var exit map[string]any
	for exit == nil {
		typ, b, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if typ == websocket.MessageText {
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			if m["type"] == "exit" {
				exit = m
			}
		}
	}
	if exit["code"] != float64(0) || exit["reason"] != "exit" {
		t.Fatalf("exit %v", exit)
	}
	if _, _, err := c.Read(ctx); websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("close %v", err)
	}
	_, out := call(t, http.MethodGet, v.srv.URL+"/api/v1/terminals/"+tid, v.person("kanishk"), "")
	if tm := out["terminal"].(map[string]any); tm["state"] != "exited" || tm["end_reason"] != "exit" || tm["ended_at"] == nil || tm["cols"] != float64(132) {
		t.Fatalf("after exit %v", tm)
	}
	v.fake.mu.Lock()
	tty, resizes := v.fake.ttys[0], v.fake.resizes
	v.fake.mu.Unlock()
	if tty.Preset != "shell" || tty.Cols != 80 || tty.Rows != 24 || len(tty.Env) != 0 || !strings.HasPrefix(tty.Session, "shell-") {
		t.Fatalf("tty request %+v: a shell carries no credential", tty)
	}
	if len(resizes) != 1 || resizes[0] != (sandbox.Resize{Cols: 132, Rows: 40}) {
		t.Fatalf("resizes %v", resizes)
	}
	if code, out := call(t, http.MethodPost, v.srv.URL+"/api/v1/terminals/"+tid+"/attach", v.person("kanishk"), ""); code != 409 {
		t.Fatalf("a ticket for an ended terminal: %d %v", code, out)
	}
}

func TestClaudeTerminalKillAndDrain(t *testing.T) {
	v := newBoxEnv(t)
	term := v.newTerminal(t, `{"preset":"claude","cols":100,"rows":30}`)
	tid := term["id"].(string)
	if term["title"] != "Claude Code (direct)" {
		t.Fatalf("title %v", term["title"])
	}
	ticket, u := v.ticket(t, tid)
	c, _, err := dial(t, u, ticket, portfolio)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if m := readText(t, c); m["type"] != "ready" {
		t.Fatal(m)
	}
	v.fake.mu.Lock()
	tty := v.fake.ttys[0]
	v.fake.mu.Unlock()
	if tty.Preset != "claude" || tty.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "sub-token" || len(tty.Env) != 1 {
		t.Fatalf("claude tty %+v", tty)
	}
	// A drain closes the socket with 4001, and the terminal lives on.
	v.drain.Start()
	for {
		_, _, err := c.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != api.CloseDrain {
				t.Fatalf("drain close %v", err)
			}
			break
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		v.fake.mu.Lock()
		h := v.fake.hangups
		v.fake.mu.Unlock()
		if h == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("orbit did not detach from sandboxd")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, out := call(t, http.MethodGet, v.srv.URL+"/api/v1/terminals/"+tid, v.person("kanishk"), "")
	if out["terminal"].(map[string]any)["state"] != "running" {
		t.Fatal("a drain ended the terminal")
	}

	code, out := call(t, http.MethodPost, v.srv.URL+"/api/v1/terminals/"+tid+"/kill", v.person("kanishk"), `{"signal":"HUP"}`)
	tm := out["terminal"].(map[string]any)
	if code != 200 || tm["state"] != "exited" || tm["end_reason"] != "killed" {
		t.Fatalf("kill: %d %v", code, out)
	}
	v.fake.mu.Lock()
	killed := v.fake.killed
	v.fake.mu.Unlock()
	if len(killed) != 1 || killed[0] != tty.Session {
		t.Fatalf("killed %v", killed)
	}
	if code, again := call(t, http.MethodPost, v.srv.URL+"/api/v1/terminals/"+tid+"/kill", v.person("kanishk"), ""); code != 200 || again["terminal"].(map[string]any)["ended_at"] != tm["ended_at"] {
		t.Fatalf("second kill: %d %v", code, again)
	}
	_, out = call(t, http.MethodGet, v.srv.URL+"/api/v1/workspaces/"+tm["workspace_id"].(string)+"/terminals?state=exited", v.person("kanishk"), "")
	if len(out["terminals"].([]any)) != 1 {
		t.Fatalf("exited %v", out)
	}
}

// TestTicketOrigins: a ticket minted without an Origin (the portfolio's
// proxy, server side) opens from either of the portfolio's origins, a ticket
// minted for one does not open from the other, and the URL's query carries
// the ticket for a client that cannot put it in a subprotocol.
func TestTicketOrigins(t *testing.T) {
	v := newBoxEnv(t)
	tid := v.newTerminal(t, `{"preset":"shell","cols":80,"rows":24}`)["id"].(string)
	ticket, u := v.ticket(t, tid)
	c, _, err := dial(t, u, ticket, "https://www.kanishksachdev.com")
	if err != nil {
		t.Fatalf("www with an origin-free ticket: %v", err)
	}
	if m := readText(t, c); m["type"] != "ready" {
		t.Fatal(m)
	}
	c.Close(websocket.StatusNormalClosure, "")

	code, out := call(t, http.MethodPost, v.srv.URL+"/api/v1/terminals/"+tid+"/attach", v.person("kanishk"), `{"origin":"`+portfolio+`"}`)
	if code != 200 {
		t.Fatal(code, out)
	}
	if _, resp, err := dial(t, u, out["ticket"].(string), "https://www.kanishksachdev.com"); err == nil || resp.StatusCode != 401 {
		t.Fatalf("a ticket for the apex opened from www: %v %v", resp, err)
	}

	ticket, u = v.ticket(t, tid)
	c, _, err = dial(t, u+"?ticket="+ticket, "", portfolio)
	if err != nil {
		t.Fatalf("ticket in the query: %v", err)
	}
	if c.Subprotocol() != api.TerminalProtocol {
		t.Fatalf("subprotocol %q", c.Subprotocol())
	}
	c.Close(websocket.StatusNormalClosure, "")
}

func TestTicketRate(t *testing.T) {
	v := newBoxEnv(t)
	tid := v.newTerminal(t, `{"preset":"shell","cols":80,"rows":24}`)["id"].(string)
	for range 10 {
		v.ticket(t, tid)
	}
	code, out := call(t, http.MethodPost, v.srv.URL+"/api/v1/terminals/"+tid+"/attach", v.person("kanishk"), "")
	if code != 429 || out["check"] != "rate" {
		t.Fatalf("an eleventh ticket in a minute: %d %v", code, out)
	}
	// Tickets are the person's: another person is not limited by them.
	if code, _ := call(t, http.MethodPost, v.srv.URL+"/api/v1/terminals/"+tid+"/attach", v.person("someone"), ""); code != 200 {
		t.Fatalf("another person: %d", code)
	}
	if code, _ := call(t, http.MethodPost, v.srv.URL+"/api/v1/terminals/"+tid+"/attach", "", ""); code != 401 {
		t.Fatalf("no token: %d", code)
	}
}

// TestTicketExpiry: a ticket past its 30 s is refused, as is one minted for
// another allowed origin.
func TestTicketExpiry(t *testing.T) {
	v := newBoxEnv(t)
	tid := v.newTerminal(t, `{"preset":"shell","cols":80,"rows":24}`)["id"].(string)
	ticket, _, err := v.s.MintTicket(ctx, tid, "kanishk", portfolio, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.s.RedeemTicket(ctx, ticket, tid, portfolio, "*"); err == nil {
		t.Fatal("an expired ticket was redeemed")
	}
	ticket, _, _ = v.s.MintTicket(ctx, tid, "kanishk", portfolio, 30*time.Second)
	if _, err := v.s.RedeemTicket(ctx, ticket, tid, "https://www.kanishksachdev.com", "*"); err == nil {
		t.Fatal("a ticket was redeemed from another origin")
	}
	if p, err := v.s.RedeemTicket(ctx, ticket, tid, portfolio, "*"); err != nil || p != "kanishk" {
		t.Fatalf("redeem: %q %v", p, err)
	}
	if _, err := v.s.RedeemTicket(ctx, ticket, tid, portfolio, "*"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second redeem: %v", err)
	}
}
