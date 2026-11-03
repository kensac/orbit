package sandboxd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/sandbox"
)

const testBoxImage = "ghcr.io/kensac/orbit-workstation@sha256:abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"

func boxConfig() Config {
	c := testConfig()
	c.BoxImage = testBoxImage
	c.WorkstationProxy, c.WorkstationNetwork = "orbit-egress-open:3128", "orbit-workstation"
	return c
}

// fakeTTY is a TTY process: it prints its pid as ttyArgv's script does,
// then echoes its input until it reads "exit" (exit 0) or is hung up.
func (f *fakeDocker) fakeTTY(execID string, conn net.Conn, r *bufio.Reader) {
	f.mu.Lock()
	f.nextPid++
	pid := f.nextPid
	f.ttys[pid] = conn
	f.mu.Unlock()
	_, _ = conn.Write([]byte(strconv.Itoa(pid) + "\r\n"))
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if err != nil {
			return
		}
		_, _ = conn.Write(buf[:n])
		if bytes.Contains(buf[:n], []byte("exit")) {
			f.mu.Lock()
			f.exits[execID] = 0
			f.mu.Unlock()
			return
		}
	}
}

// fakeCommand answers the fixed tmux and kill commands a box runs.
func (f *fakeDocker) fakeCommand(execID string, cmd []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code := 0
	switch {
	case len(cmd) == 4 && cmd[1] == "has-session":
		if !f.sessions[strings.TrimPrefix(cmd[3], "=")] {
			code = 1
		}
	case len(cmd) == 4 && cmd[1] == "kill-session":
		delete(f.sessions, strings.TrimPrefix(cmd[3], "="))
	case cmd[0] == "kill":
		pid, _ := strconv.Atoi(cmd[2])
		if c := f.ttys[pid]; c != nil {
			_ = c.Close()
		}
	}
	f.exits[execID] = code
}

// TestBoxSpecDocument pins the whole create request of a box: what it
// keeps of a build's options and what it gives up, reviewed as the security
// change any edit to it is.
func TestBoxSpecDocument(t *testing.T) {
	b, err := json.Marshal(boxSpec(boxConfig(), "orbit", nil))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	proxy := "http://orbit-egress-open:3128"
	vol := func(which, target string) map[string]any {
		return map[string]any{"Type": "volume", "Source": "orbit-box-orbit-" + which, "Target": target, "ReadOnly": false,
			"VolumeOptions": map[string]any{"NoCopy": false, "Labels": map[string]any{"orbit.box": "orbit"}}}
	}
	want := map[string]any{
		"Image":      testBoxImage,
		"User":       "10001:10001",
		"Entrypoint": []any{"sleep", "infinity"},
		"Cmd":        nil,
		"Env": []any{
			"HOME=/home/agent", "LANG=en_US.UTF-8", "TERM=xterm-256color",
			"HTTPS_PROXY=" + proxy, "HTTP_PROXY=" + proxy, "https_proxy=" + proxy, "http_proxy=" + proxy,
			"NO_PROXY=", "no_proxy=", "DISABLE_AUTOUPDATER=1",
		},
		"WorkingDir":  "/work",
		"Hostname":    "orbit",
		"Labels":      map[string]any{"orbit.box": "orbit"},
		"AttachStdin": false, "AttachStdout": false, "AttachStderr": false,
		"OpenStdin": false, "Tty": false, "NetworkDisabled": false,
		"HostConfig": map[string]any{
			"Privileged": false,
			// apt and sudo write the root filesystem; it lasts as long as
			// the container (until the image pin moves).
			"ReadonlyRootfs": false,
			"CapAdd":         nil,
			// Docker's default set less these: sudo needs setuid and
			// setgid, and with no-new-privileges it would not run at all.
			"CapDrop":         []any{"MKNOD", "NET_RAW"},
			"SecurityOpt":     nil,
			"NetworkMode":     "orbit-workstation",
			"IpcMode":         "private",
			"PidMode":         "",
			"UTSMode":         "",
			"UsernsMode":      "",
			"CgroupnsMode":    "private",
			"Binds":           nil,
			"Mounts":          []any{vol("home", "/home/agent"), vol("work", "/work")},
			"Tmpfs":           nil,
			"Devices":         nil,
			"PublishAllPorts": false,
			"PortBindings":    nil,
			"ExtraHosts":      nil,
			"Memory":          float64(8 << 30),
			"MemorySwap":      float64(8 << 30),
			"NanoCpus":        float64(4_000_000_000),
			"PidsLimit":       float64(4096),
			"Ulimits":         []any{map[string]any{"Name": "nofile", "Soft": float64(65536), "Hard": float64(65536)}},
			"OomScoreAdj":     float64(300),
			"Init":            true,
			"AutoRemove":      false,
			"RestartPolicy":   map[string]any{"Name": "unless-stopped"},
		},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{"orbit-workstation": map[string]any{}}},
	}
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("box create request changed:\n%s", gb)
	}
	cfg := boxConfig()
	cfg.BoxRuntime = "runsc"
	if rt := boxSpec(cfg, "orbit", nil).HostConfig.Runtime; rt != "runsc" {
		t.Fatalf("runtime %q", rt)
	}
}

// TestTTYExecSpec pins both presets' exec: a fixed script, the session as
// its argument, a TTY of the asked size, the credential only when given.
func TestTTYExecSpec(t *testing.T) {
	got := ttyExecSpec(sandbox.TTYRequest{Preset: "claude", Session: "claude-1a2b3c4d", Cols: 120, Rows: 32,
		Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}})
	want := execCreate{
		AttachStdin: true, AttachStdout: true, AttachStderr: false, Tty: true,
		User: "10001:10001", WorkingDir: "/work",
		Cmd:         []string{"sh", "-c", ttyScript, "orbit-tty", "claude-1a2b3c4d", "claude"},
		Env:         []string{"CLAUDE_CODE_OAUTH_TOKEN=tok", "TERM=xterm-256color"},
		ConsoleSize: []int{32, 120},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("claude exec %+v", got)
	}
	got = ttyExecSpec(sandbox.TTYRequest{Preset: "shell", Session: "sh-1", Cwd: "conversations/x", Cols: 80, Rows: 24})
	want = execCreate{
		AttachStdin: true, AttachStdout: true, Tty: true,
		User: "10001:10001", WorkingDir: "/work/conversations/x",
		Cmd:         []string{"sh", "-c", ttyScript, "orbit-tty", "sh-1"},
		Env:         []string{"TERM=xterm-256color"},
		ConsoleSize: []int{24, 80},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shell exec %+v", got)
	}
	// The request has no field for an argv, a user or privileges.
	tt := reflect.TypeOf(sandbox.TTYRequest{})
	var fields []string
	for i := range tt.NumField() {
		fields = append(fields, tt.Field(i).Name)
	}
	if !reflect.DeepEqual(fields, []string{"Preset", "Session", "Cwd", "Cols", "Rows", "Env", "Account", "MCPServer"}) {
		t.Fatalf("TTYRequest fields %v: a new caller-controlled field is a security change", fields)
	}
}

func TestTTYRequestValidate(t *testing.T) {
	ok := sandbox.TTYRequest{Preset: "shell", Session: "sh-1", Cols: 80, Rows: 24}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]func(*sandbox.TTYRequest){
		"preset":        func(r *sandbox.TTYRequest) { r.Preset = "bash" },
		"session colon": func(r *sandbox.TTYRequest) { r.Session = "a:b" },
		"session flag":  func(r *sandbox.TTYRequest) { r.Session = "-t" },
		"cwd up":        func(r *sandbox.TTYRequest) { r.Cwd = "../etc" },
		"cwd absolute":  func(r *sandbox.TTYRequest) { r.Cwd = "/etc" },
		"cwd unclean":   func(r *sandbox.TTYRequest) { r.Cwd = "a/../b" },
		"no size":       func(r *sandbox.TTYRequest) { r.Cols = 0 },
		"huge":          func(r *sandbox.TTYRequest) { r.Rows = 100000 },
		"env":           func(r *sandbox.TTYRequest) { r.Env = map[string]string{"LD_PRELOAD": "/x"} },
	} {
		r := ok
		m(&r)
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestVerifyBox(t *testing.T) {
	cfg := boxConfig()
	good := func() containerState {
		var c containerState
		c.Config.User = "10001:10001"
		c.Config.Labels = map[string]string{LabelBox: "orbit"}
		c.HostConfig.CapDrop = []string{"MKNOD", "NET_RAW"}
		c.HostConfig.NetworkMode = "orbit-workstation"
		c.HostConfig.IpcMode = "private"
		c.HostConfig.Memory = BoxMemoryBytes
		p := int64(BoxPids)
		c.HostConfig.PidsLimit = &p
		c.Mounts = []mountState{{"volume", "orbit-box-orbit-home", "/home/agent"}, {"volume", "orbit-box-orbit-work", "/work"}}
		return c
	}
	if err := verifyBox(cfg, "orbit", good()); err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]func(*containerState){
		"another agent's": func(c *containerState) { c.Config.Labels[LabelBox] = "finance" },
		"a sandbox":       func(c *containerState) { c.Config.Labels[LabelManaged] = "1" },
		"root":            func(c *containerState) { c.Config.User = "0:0" },
		"privileged":      func(c *containerState) { c.HostConfig.Privileged = true },
		"cap added":       func(c *containerState) { c.HostConfig.CapAdd = []string{"SYS_ADMIN"} },
		"raw sockets":     func(c *containerState) { c.HostConfig.CapDrop = []string{"MKNOD"} },
		"host network":    func(c *containerState) { c.HostConfig.NetworkMode = "host" },
		"build network":   func(c *containerState) { c.HostConfig.NetworkMode = "orbit-sandbox" },
		"bind":            func(c *containerState) { c.HostConfig.Binds = []string{"/:/host"} },
		"host pid":        func(c *containerState) { c.HostConfig.PidMode = "host" },
		"other volume":    func(c *containerState) { c.Mounts[1].Name = "root_plaid_db_data_v18" },
		"bind mount":      func(c *containerState) { c.Mounts[0].Type = "bind" },
		"extra mount":     func(c *containerState) { c.Mounts = append(c.Mounts, mountState{"volume", "x", "/etc"}) },
	} {
		c := good()
		m(&c)
		if err := verifyBox(cfg, "orbit", c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBoxLifecycle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.client.EnsureBox(ctx, "orbit", nil); err == nil {
		t.Fatal("a box was made without ORBIT_SANDBOX_BOX_IMAGE")
	}
	h.srv.cfg = boxConfig()
	h.srv.cfg.Docker, h.srv.cfg.Listen = h.srv.d.sock, "unix:"+h.sock

	st, err := h.client.Box(ctx, "orbit")
	if err != nil || st.State != sandbox.BoxNone {
		t.Fatalf("before: %+v %v", st, err)
	}
	st, err = h.client.EnsureBox(ctx, "orbit", nil)
	if err != nil || st.State != sandbox.BoxRunning || st.Image != testBoxImage || !sandbox.ValidID(st.ID) {
		t.Fatalf("ensure: %+v %v", st, err)
	}
	h.fd.mu.Lock()
	created, query := h.fd.created[0], h.fd.createQueries[0]
	h.fd.mu.Unlock()
	if mustJSON(t, created) != mustJSON(t, boxSpec(h.srv.cfg, "orbit", nil)) || query != "name=orbit-box-orbit" {
		t.Fatalf("docker received %s for %s", mustJSON(t, created), query)
	}
	again, err := h.client.EnsureBox(ctx, "orbit", nil)
	if err != nil || again.ID != st.ID || len(h.fd.created) != 1 {
		t.Fatalf("second ensure made another: %+v %v", again, err)
	}
	// A box is never a sandbox: not counted, not reaped.
	h.srv.Reap(ctx)
	if h.fd.containers[st.ID] == nil {
		t.Fatal("the reaper removed a box")
	}
	if _, err := h.client.Create(ctx, "run-1", workTar(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Create(ctx, "run-2", workTar(t)); err != nil {
		t.Fatalf("a box counted against MaxSandboxes: %v", err)
	}
	if err := h.client.Destroy(ctx, st.ID); err != nil || h.fd.containers[st.ID] == nil {
		t.Fatalf("the sandbox API reached a box: %v", err)
	}

	stopped, err := h.client.StopBox(ctx, "orbit")
	if err != nil || stopped.State != sandbox.BoxStopped {
		t.Fatalf("stop: %+v %v", stopped, err)
	}
	if _, err := h.client.AttachTTY(ctx, "orbit", sandbox.TTYRequest{Preset: "shell", Session: "sh-1", Cols: 80, Rows: 24}); err == nil {
		t.Fatal("a TTY attached to a stopped box")
	}
	// A new image pin reaches a stopped box: recreated, volumes by name.
	newImage := strings.Replace(testBoxImage, "abcdef", "012345", 1)
	h.srv.cfg.BoxImage = newImage
	st2, err := h.client.EnsureBox(ctx, "orbit", nil)
	if err != nil || st2.State != sandbox.BoxRunning || st2.Image != newImage || st2.ID == st.ID {
		t.Fatalf("recreate: %+v %v", st2, err)
	}
	if h.fd.containers[st.ID] != nil {
		t.Fatal("the old container was kept")
	}
	// A running box keeps its image when the pin moves.
	h.srv.cfg.BoxImage = testBoxImage
	if st3, _ := h.client.EnsureBox(ctx, "orbit", nil); st3.ID != st2.ID {
		t.Fatal("a running box was recreated")
	}

	// A container that is not this box, under its name, is refused.
	h.fd.mu.Lock()
	h.fd.containers[st2.ID].labels = map[string]string{"com.docker.compose.service": "orbit"}
	h.fd.mu.Unlock()
	var se *sandbox.Error
	if _, err := h.client.EnsureBox(ctx, "orbit", nil); !errors.As(err, &se) || se.Status != http.StatusConflict {
		t.Fatalf("foreign container: %v", err)
	}
	if _, err := h.client.Box(ctx, "Bad/Name"); err == nil {
		t.Fatal("a bad name was accepted")
	}
}

func TestBoxTTY(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg = boxConfig()
	h.srv.cfg.Docker = h.srv.d.sock
	ctx := context.Background()
	if _, err := h.client.EnsureBox(ctx, "orbit", nil); err != nil {
		t.Fatal(err)
	}
	tty, err := h.client.AttachTTY(ctx, "orbit", sandbox.TTYRequest{Preset: "claude", Session: "claude-1", Cols: 100, Rows: 30,
		Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := tty.Resize(132, 40); err != nil {
		t.Fatal(err)
	}
	if _, err := tty.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(tty, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("echo %q %v: the pid line must not reach orbit", buf, err)
	}
	_, _ = tty.Write([]byte("exit"))
	rest, err := io.ReadAll(tty)
	if err != nil || string(rest) != "exit" || tty.Exit == nil || tty.Exit.ExitCode != 0 {
		t.Fatalf("rest %q err %v exit %+v", rest, err, tty.Exit)
	}
	tty.Close()
	h.fd.mu.Lock()
	var execID string
	for id, e := range h.fd.execs {
		if e.Tty {
			execID = id
		}
	}
	resizes := h.fd.resizes[execID]
	h.fd.mu.Unlock()
	if !reflect.DeepEqual(resizes, []string{"132x40"}) {
		t.Fatalf("resizes %v", resizes)
	}

	// Detaching hangs up the tmux client, not the session.
	tty, err = h.client.AttachTTY(ctx, "orbit", sandbox.TTYRequest{Preset: "shell", Session: "sh-1", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	tty.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.fd.mu.Lock()
		hungUp := false
		for _, e := range h.fd.execs {
			if len(e.Cmd) == 3 && e.Cmd[0] == "kill" && e.Cmd[1] == "-HUP" {
				hungUp = true
			}
		}
		h.fd.mu.Unlock()
		if hungUp {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a detach did not hang up the tmux client")
		}
		time.Sleep(10 * time.Millisecond)
	}

	h.fd.mu.Lock()
	h.fd.sessions["sh-1"] = true
	h.fd.mu.Unlock()
	if ok, err := h.client.SessionExists(ctx, "orbit", "sh-1"); err != nil || !ok {
		t.Fatalf("exists: %v %v", ok, err)
	}
	if err := h.client.KillSession(ctx, "orbit", "sh-1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := h.client.SessionExists(ctx, "orbit", "sh-1"); err != nil || ok {
		t.Fatalf("after kill: %v %v", ok, err)
	}
	h.fd.mu.Lock()
	for _, e := range h.fd.execs {
		if e.Cmd[0] == "tmux" && (len(e.Cmd) != 4 || !strings.HasPrefix(e.Cmd[3], "=")) {
			t.Errorf("tmux command %v without an exact target", e.Cmd)
		}
	}
	h.fd.mu.Unlock()
}
