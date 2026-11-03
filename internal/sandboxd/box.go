package sandboxd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/sandbox"
)

// A box is an agent's persistent workstation (docs/workspace.md, The box):
// one container per agent from the workstation image, its home and /work
// on named volumes that outlive the container. Unlike a build sandbox it
// has no lifetime, a writable root and docker's default capabilities, and
// no no-new-privileges, because the image's passwordless sudo (apt, an
// inner dockerd later) needs setuid to work. Everything else is fixed here
// as for a build: the uid, the internal network with the workstation
// egress proxy as its only route out, no host namespace, no bind mount,
// resource limits.
const (
	// LabelBox carries the agent's name. A box never carries LabelManaged,
	// so the reaper and MaxSandboxes leave it alone.
	LabelBox = "orbit.box"
	// BoxMemoryBytes, BoxNanoCPUs, BoxPids: a laptop's share of hetzner
	// (32 GB, 16 vCPU) or oci (24 GB, 4 vCPU) for one person's terminals,
	// Claude Code and a build or two.
	BoxMemoryBytes = 8 << 30
	BoxNanoCPUs    = 4_000_000_000
	BoxPids        = 4096
	// BoxNoFile is the open-file limit: editors' watchers and node use many.
	BoxNoFile = 65536
)

// boxCapDrop is what a box drops from docker's default capability set:
// raw sockets and device nodes, which nothing in a workstation needs.
var boxCapDrop = []string{"MKNOD", "NET_RAW"}

func boxContainer(name string) string { return "orbit-box-" + name }

func boxVolume(name, which string) string { return "orbit-box-" + name + "-" + which }

// accountVolume is an account's volume: its credentials and its private
// runtime state live there and nowhere else (docs/agents/security.md).
func accountVolume(name, slot string) string { return boxVolume(name, "acct-"+slot) }

// boxSpec is the one container a box is. Its inputs are sandboxd's
// configuration, the agent's name (ValidBoxName) and the account slots it
// mounts (sandbox.ValidSlot, sorted and unique).
func boxSpec(cfg Config, name string, accounts []string) containerCreate {
	proxy := "http://" + cfg.WorkstationProxy
	vol := func(source, target string) mount {
		return mount{Type: "volume", Source: source, Target: target,
			VolumeOptions: volumeOptions{Labels: map[string]string{LabelBox: name}}}
	}
	mounts := []mount{vol(boxVolume(name, "home"), sandbox.BoxHome), vol(boxVolume(name, "work"), sandbox.WorkDir)}
	for _, a := range accounts {
		mounts = append(mounts, vol(accountVolume(name, a), sandbox.AccountDir(a)))
	}
	return containerCreate{
		Image: cfg.BoxImage,
		User:  user,
		// Nothing runs as PID 1 but docker's init and this: terminals and
		// agent turns are execs beside it.
		Entrypoint: []string{"sleep", "infinity"},
		Env: []string{
			"HOME=" + sandbox.BoxHome,
			"LANG=en_US.UTF-8",
			"TERM=xterm-256color",
			"HTTPS_PROXY=" + proxy,
			"HTTP_PROXY=" + proxy,
			"https_proxy=" + proxy,
			"http_proxy=" + proxy,
			"NO_PROXY=",
			"no_proxy=",
			"DISABLE_AUTOUPDATER=1",
		},
		WorkingDir: sandbox.WorkDir,
		Hostname:   name,
		Labels:     map[string]string{LabelBox: name},
		HostConfig: hostConfig{
			CapDrop:      append([]string(nil), boxCapDrop...),
			NetworkMode:  cfg.WorkstationNetwork,
			IpcMode:      "private",
			CgroupnsMode: "private",
			// Named volumes, filled from the image on first use (NoCopy
			// false), so the home starts as the image's. An account's
			// volume is mounted inside the home, at its directory.
			Mounts:     mounts,
			Memory:     BoxMemoryBytes,
			MemorySwap: BoxMemoryBytes,
			NanoCpus:   BoxNanoCPUs,
			PidsLimit:  BoxPids,
			Ulimits:    []ulimit{{Name: "nofile", Soft: BoxNoFile, Hard: BoxNoFile}},
			// Below a build (500): a person's box outlives a build's need.
			OomScoreAdj: 300,
			Init:        true,
			// A host or daemon restart brings it back; a stop keeps it down.
			RestartPolicy: restartPolicy{Name: "unless-stopped"},
			Runtime:       cfg.BoxRuntime,
		},
		NetworkingConfig: networkingConfig{EndpointsConfig: map[string]struct{}{cfg.WorkstationNetwork: {}}},
	}
}

// verifyBox checks that a container is the box sandboxd makes for name,
// whether it was just created or found from an earlier start.
func verifyBox(cfg Config, name string, c containerState) error {
	h := c.HostConfig
	switch {
	case c.Config.Labels[LabelBox] != name:
		return fmt.Errorf("label %s is %q, want %q", LabelBox, c.Config.Labels[LabelBox], name)
	case c.Config.Labels[LabelManaged] != "":
		return fmt.Errorf("a box must not carry %s", LabelManaged)
	case c.Config.User != user:
		return fmt.Errorf("user is %q, want %q", c.Config.User, user)
	case h.Privileged:
		return errors.New("container is privileged")
	case len(h.CapAdd) != 0:
		return fmt.Errorf("capabilities added: %v", h.CapAdd)
	case !contains(h.CapDrop, "MKNOD") || !contains(h.CapDrop, "NET_RAW"):
		return fmt.Errorf("capabilities dropped: %v, want %v", h.CapDrop, boxCapDrop)
	case h.NetworkMode != cfg.WorkstationNetwork:
		return fmt.Errorf("network is %q, want %q", h.NetworkMode, cfg.WorkstationNetwork)
	case len(h.Binds) != 0:
		return fmt.Errorf("bind mounts present: %v", h.Binds)
	case h.PidMode != "" || h.UsernsMode != "":
		return fmt.Errorf("host namespace shared (pid %q, userns %q)", h.PidMode, h.UsernsMode)
	case h.IpcMode != "private":
		return fmt.Errorf("ipc mode is %q", h.IpcMode)
	case h.Memory != BoxMemoryBytes:
		return fmt.Errorf("memory limit is %d", h.Memory)
	case h.PidsLimit == nil || *h.PidsLimit != BoxPids:
		return fmt.Errorf("pids limit is not %d", BoxPids)
	}
	want := map[string]string{sandbox.BoxHome: boxVolume(name, "home"), sandbox.WorkDir: boxVolume(name, "work")}
	for _, m := range c.Mounts {
		v := want[m.Destination]
		if slot, ok := strings.CutPrefix(m.Destination, sandbox.AccountsRoot+"/"); ok && sandbox.ValidSlot(slot) {
			v = accountVolume(name, slot)
		}
		if m.Type != "volume" || v == "" || m.Name != v {
			return fmt.Errorf("mount of %s %q at %s", m.Type, m.Name, m.Destination)
		}
	}
	return nil
}

// mountedAccounts are the account slots a verified box mounts, sorted.
func mountedAccounts(c containerState) []string {
	var out []string
	for _, m := range c.Mounts {
		if slot, ok := strings.CutPrefix(m.Destination, sandbox.AccountsRoot+"/"); ok && sandbox.ValidSlot(slot) {
			out = append(out, slot)
		}
	}
	slices.Sort(out)
	return out
}

// mergeAccounts is the sorted union of what a box mounts and what a request
// names: a recreate never unmounts an account.
func mergeAccounts(a, b []string) []string {
	out := slices.Concat(a, b)
	slices.Sort(out)
	return slices.Compact(out)
}

// ttyScript starts the tmux client: it prints its pid first, so a detach
// can hang up exactly that client (others on the session stay attached),
// then runs tmux with the arguments after the session name. The session
// name and every argument are positional, never part of the script.
const ttyScript = `echo "$$"; s=$1; shift; exec tmux new-session -A -s "$s" "$@"`

// accountScript runs a command with an account's own login: the
// subscription token and every ANTHROPIC_ variable are removed from its
// environment, whatever the tmux server inherited.
const accountScript = `unset CLAUDE_CODE_OAUTH_TOKEN; for v in $(env | sed -n 's/^\(ANTHROPIC_[A-Za-z0-9_]*\)=.*/\1/p'); do unset "$v"; done; exec "$@"`

// ttyArgv is a preset's command, for the box's own profile or for an
// account (its slot). An account's session has CLAUDE_CONFIG_DIR set to its
// directory and the subscription token marked removed (so windows opened in
// it later lack it too), and its command runs through accountScript.
func ttyArgv(preset, session, account string) []string {
	argv := []string{"sh", "-c", ttyScript, "orbit-tty", session}
	var cmd []string
	switch preset {
	case sandbox.TTYClaude:
		cmd = []string{"claude"}
	case sandbox.TTYLogin:
		cmd = []string{"claude", "auth", "login"}
	}
	if account == "" {
		return append(argv, cmd...)
	}
	if cmd == nil {
		cmd = []string{"bash", "-l"}
	}
	argv = append(argv, "-e", "CLAUDE_CONFIG_DIR="+sandbox.AccountDir(account), "sh", "-c", accountScript, "orbit-account")
	argv = append(argv, cmd...)
	return append(argv, ";", "set-environment", "-t", "="+session, "-r", "CLAUDE_CODE_OAUTH_TOKEN")
}

// ttyExecSpec is the one TTY process shape: a preset's argv, the allowed
// environment and the terminal type, everything else fixed.
func ttyExecSpec(tr sandbox.TTYRequest) execCreate {
	e := execSpec(sandbox.AttachRequest{Argv: ttyArgv(tr.Preset, tr.Session, tr.Account), Env: tr.Env})
	e.Tty = true
	e.AttachStderr = false
	e.Env = append(e.Env, "TERM=xterm-256color")
	e.WorkingDir = path.Join(sandbox.WorkDir, tr.Cwd)
	e.ConsoleSize = []int{tr.Rows, tr.Cols}
	return e
}

func (s *Server) boxesOn() error {
	if s.cfg.BoxImage == "" {
		return errors.New("boxes are not configured here (ORBIT_SANDBOX_BOX_IMAGE)")
	}
	return nil
}

// boxState reads a box's container: errNotFound when there is none, an
// error when a container of that name is not a box sandboxd made.
func (s *Server) boxState(ctx context.Context, name string) (containerState, error) {
	c, err := s.d.inspect(ctx, boxContainer(name))
	if err != nil {
		return c, err
	}
	if c.Config.Labels[LabelChatBox] != "" {
		return c, fmt.Errorf("container %s is an isolated chat box", boxContainer(name))
	}
	if err := verifyBox(s.cfg, name, c); err != nil {
		return c, fmt.Errorf("container %s is not this box: %w", boxContainer(name), err)
	}
	return c, nil
}

func (s *Server) status(name string, c *containerState) sandbox.BoxStatus {
	st := sandbox.BoxStatus{Name: name, State: sandbox.BoxNone, Host: s.host, Arch: s.arch}
	if c == nil {
		return st
	}
	st.ID, st.Image, st.State = c.ID, c.Config.Image, sandbox.BoxStopped
	st.Accounts = mountedAccounts(*c)
	if c.State.Running {
		st.State = sandbox.BoxRunning
	}
	if t, err := time.Parse(time.RFC3339Nano, c.Created); err == nil {
		st.CreatedAt = &t
	}
	return st
}

func boxName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !sandbox.ValidBoxName(name) {
		writeError(w, http.StatusBadRequest, "a box is named for its agent: lowercase letters, digits and dashes")
		return "", false
	}
	return name, true
}

func (s *Server) boxGet(w http.ResponseWriter, r *http.Request) {
	name, ok := boxName(w, r)
	if !ok {
		return
	}
	c, err := s.boxState(r.Context(), name)
	switch {
	case errors.Is(err, errNotFound):
		writeJSON(w, http.StatusOK, s.status(name, nil))
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeJSON(w, http.StatusOK, s.status(name, &c))
	}
}

// ensureRequest reads a box request's optional body.
func ensureRequest(w http.ResponseWriter, r *http.Request) (sandbox.EnsureRequest, bool) {
	var req sandbox.EnsureRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "box request: "+err.Error())
		return req, false
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return req, false
	}
	return req, true
}

// boxEnsure creates the box, or starts it. A stopped box whose image is no
// longer the configured one, or that lacks an account asked for, is
// recreated (its volumes, home, work and accounts, are kept, and it mounts
// every account it did); a running one keeps its image and mounts until it
// is stopped, so moving the pin never cuts a person's terminals.
func (s *Server) boxEnsure(w http.ResponseWriter, r *http.Request) {
	s.boxUp(w, r, false)
}

// boxRecreate replaces the container whatever its state, for a mount a
// running box lacks. Its volumes stay.
func (s *Server) boxRecreate(w http.ResponseWriter, r *http.Request) {
	s.boxUp(w, r, true)
}

func (s *Server) boxUp(w http.ResponseWriter, r *http.Request, recreate bool) {
	name, ok := boxName(w, r)
	if !ok {
		return
	}
	req, ok := ensureRequest(w, r)
	if !ok {
		return
	}
	if err := s.boxesOn(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	s.boxMu.Lock()
	defer s.boxMu.Unlock()
	accounts := mergeAccounts(nil, req.Accounts)
	c, err := s.boxState(ctx, name)
	switch {
	case errors.Is(err, errNotFound):
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
		return
	case c.State.Running && !recreate:
		writeJSON(w, http.StatusOK, s.status(name, &c))
		return
	default:
		mounted := mountedAccounts(c)
		accounts = mergeAccounts(mounted, req.Accounts)
		if recreate || c.Config.Image != s.cfg.BoxImage || !slices.Equal(accounts, mounted) {
			if err := s.d.remove(ctx, c.ID); err != nil {
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			s.log.Info("sandboxd: box recreated", "box", name, "image", s.cfg.BoxImage, "accounts", len(accounts))
			err = errNotFound
		}
	}
	if len(accounts) > sandbox.MaxAccounts {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("a box mounts at most %d accounts", sandbox.MaxAccounts))
		return
	}
	if errors.Is(err, errNotFound) {
		id, err := s.d.createNamed(ctx, boxContainer(name), boxSpec(s.cfg, name, accounts))
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if c, err = s.boxState(ctx, name); err != nil {
			// Named volumes stay: they are the box's home, work and accounts.
			if rerr := s.d.remove(context.WithoutCancel(ctx), id); rerr != nil {
				s.log.Error("sandboxd: remove after a failed box create", "box", name, "err", rerr)
			}
			writeError(w, http.StatusInternalServerError, "the daemon did not create the box asked for: "+err.Error())
			return
		}
		s.log.Info("sandboxd: box created", "box", name, "id", id, "image", s.cfg.BoxImage, "runtime", s.cfg.BoxRuntime, "accounts", len(accounts))
	}
	if err := s.d.start(ctx, c.ID); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	c.State.Running = true
	s.log.Info("sandboxd: box started", "box", name, "id", c.ID)
	writeJSON(w, http.StatusOK, s.status(name, &c))
}

func (s *Server) boxStop(w http.ResponseWriter, r *http.Request) {
	name, ok := boxName(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	s.boxMu.Lock()
	defer s.boxMu.Unlock()
	c, err := s.boxState(ctx, name)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, sandbox.ErrNoBox.Error())
		return
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if c.State.Running {
		if err := s.d.stop(ctx, c.ID); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		c.State.Running = false
		s.log.Info("sandboxd: box stopped", "box", name, "id", c.ID)
	}
	writeJSON(w, http.StatusOK, s.status(name, &c))
}

// runningBox is the box's container for an exec, or an error written.
func (s *Server) runningBox(w http.ResponseWriter, r *http.Request) (containerState, string, bool) {
	name, ok := boxName(w, r)
	if !ok {
		return containerState{}, "", false
	}
	c, err := s.boxState(r.Context(), name)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, sandbox.ErrNoBox.Error())
		return c, name, false
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
		return c, name, false
	case !c.State.Running:
		writeError(w, http.StatusConflict, "the box is stopped")
		return c, name, false
	}
	return c, name, true
}

// tmux runs one fixed tmux command in a box and returns its exit code.
func (s *Server) tmux(ctx context.Context, id string, args ...string) (int, error) {
	e := execSpec(sandbox.AttachRequest{Argv: append([]string{"tmux"}, args...)})
	e.AttachStdin = false
	execID, err := s.d.createExec(ctx, id, e)
	if err != nil {
		return -1, err
	}
	conn, br, err := s.d.startExec(ctx, execID)
	if err != nil {
		return -1, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(br, 1<<20))
	_ = conn.Close()
	return s.exitCode(execID)
}

// tmuxOutput runs a fixed tmux query and bounds the bytes returned to orbit.
func (s *Server) tmuxOutput(ctx context.Context, id string, limit int64, args ...string) ([]byte, int, error) {
	e := execSpec(sandbox.AttachRequest{Argv: append([]string{"tmux"}, args...)})
	e.AttachStdin = false
	execID, err := s.d.createExec(ctx, id, e)
	if err != nil {
		return nil, -1, err
	}
	conn, br, err := s.d.startExec(ctx, execID)
	if err != nil {
		return nil, -1, err
	}
	var captured bytes.Buffer
	readErr := demux(br, func(typ byte, p []byte) error {
		if typ != sandbox.FrameStdout {
			return nil
		}
		remaining := limit - int64(captured.Len())
		if remaining > 0 {
			_, _ = captured.Write(p[:min(int64(len(p)), remaining)])
		}
		return nil
	})
	_ = conn.Close()
	if readErr != nil {
		return nil, -1, readErr
	}
	code, err := s.exitCode(execID)
	return captured.Bytes(), code, err
}

func session(w http.ResponseWriter, r *http.Request) (string, bool) {
	ss := r.PathValue("session")
	if !sandbox.ValidSession(ss) {
		writeError(w, http.StatusBadRequest, "session: lowercase letters, digits and dashes")
		return "", false
	}
	return ss, true
}

func (s *Server) sessionExists(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.runningBox(w, r)
	if !ok {
		return
	}
	ss, ok := session(w, r)
	if !ok {
		return
	}
	// "=" is tmux's exact match; without it "a" would match "ab".
	code, err := s.tmux(r.Context(), c.ID, "has-session", "-t", "="+ss)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"exists": code == 0})
}

func (s *Server) sessionKill(w http.ResponseWriter, r *http.Request) {
	c, name, ok := s.runningBox(w, r)
	if !ok {
		return
	}
	ss, ok := session(w, r)
	if !ok {
		return
	}
	// A session already gone (exit 1) is the outcome asked for.
	if _, err := s.tmux(r.Context(), c.ID, "kill-session", "-t", "="+ss); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.log.Info("sandboxd: tmux session killed", "box", name, "session", ss)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sessionScrollback(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.runningBox(w, r)
	if !ok {
		return
	}
	ss, ok := session(w, r)
	if !ok {
		return
	}
	lines := 10000
	if v := r.URL.Query().Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10000 {
			writeError(w, http.StatusBadRequest, "lines: want 1 to 10000")
			return
		}
		lines = n
	}
	b, code, err := s.tmuxOutput(r.Context(), c.ID, 4<<20, "capture-pane", "-p", "-e", "-S", "-"+strconv.Itoa(lines), "-t", "="+ss)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if code != 0 {
		writeError(w, http.StatusNotFound, "no such terminal session")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b) //nolint:gosec // G705: this is raw terminal data, never HTML.
}

func (s *Server) sessionResize(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.runningBox(w, r)
	if !ok {
		return
	}
	ss, ok := session(w, r)
	if !ok {
		return
	}
	var size sandbox.Resize
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&size); err != nil {
		writeError(w, http.StatusBadRequest, "resize: "+err.Error())
		return
	}
	if err := size.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	code, err := s.tmux(r.Context(), c.ID, "resize-window", "-t", "="+ss, "-x", strconv.Itoa(size.Cols), "-y", strconv.Itoa(size.Rows))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if code != 0 {
		writeError(w, http.StatusNotFound, "no such terminal session")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// tty attaches a TTY to a tmux session: orbit's frames are input and
// resizes, the terminal's output goes back as stdout frames, then an exit
// frame. When orbit goes away the tmux client is hung up (the session
// lives on); when the client ends (the session exited, or a detach) the
// stream ends with its exit code.
func (s *Server) tty(w http.ResponseWriter, r *http.Request) {
	c, name, ok := s.runningBox(w, r)
	if !ok {
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), sandbox.TTYUpgrade) {
		writeError(w, http.StatusUpgradeRequired, "tty upgrades to "+sandbox.TTYUpgrade)
		return
	}
	var tr sandbox.TTYRequest
	body := http.MaxBytesReader(w, r.Body, 64<<10)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&tr); err != nil {
		writeError(w, http.StatusBadRequest, "tty request: "+err.Error())
		return
	}
	if err := tr.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		writeError(w, http.StatusBadRequest, "tty request: "+err.Error())
		return
	}
	if tr.Account != "" && !slices.Contains(mountedAccounts(c), tr.Account) {
		writeError(w, http.StatusConflict, "the box does not mount account "+tr.Account+": recreate it")
		return
	}
	ctx := r.Context()
	execID, err := s.d.createExec(ctx, c.ID, ttyExecSpec(tr))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	dconn, dbr, err := s.d.startExecTTY(ctx, execID, tr.Cols, tr.Rows)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer func() { _ = dconn.Close() }()
	pid, err := readPid(dconn, dbr)
	if err != nil {
		writeError(w, http.StatusBadGateway, "tty: "+err.Error())
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "connection cannot be upgraded")
		return
	}
	cconn, cbuf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = cconn.Close() }()
	_, err = cbuf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + sandbox.TTYUpgrade + "\r\n\r\n")
	if err == nil {
		err = cbuf.Flush()
	}
	if err != nil {
		s.hangUp(c.ID, pid)
		return
	}
	s.log.Info("sandboxd: tty attached", "box", name, "session", tr.Session, "preset", tr.Preset, "account", tr.Account, "exec", execID)

	// orbit's side: input and resizes until it closes, then hang up the
	// tmux client so its exec ends.
	go func() {
		defer s.hangUp(c.ID, pid)
		for {
			typ, p, err := sandbox.ReadFrame(cbuf.Reader)
			if err != nil {
				return
			}
			switch typ {
			case sandbox.FrameStdin:
				if _, err := dconn.Write(p); err != nil {
					return
				}
			case sandbox.FrameResize:
				var rz sandbox.Resize
				if json.Unmarshal(p, &rz) != nil || rz.Validate() != nil {
					continue
				}
				rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := s.d.resizeExec(rctx, execID, rz.Cols, rz.Rows); err != nil {
					s.log.Warn("sandboxd: tty resize", "box", name, "err", err)
				}
				cancel()
			default:
				return
			}
		}
	}()

	exit := sandbox.Exit{ExitCode: -1}
	buf := make([]byte, 32<<10)
	for {
		n, rerr := dbr.Read(buf)
		if n > 0 {
			if err := sandbox.WriteFrame(cconn, sandbox.FrameStdout, buf[:n]); err != nil {
				s.log.Info("sandboxd: tty detached", "box", name, "session", tr.Session)
				return
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				exit.Error = "terminal stream: " + rerr.Error()
			}
			break
		}
	}
	if exit.Error == "" {
		code, err := s.exitCode(execID)
		if err != nil {
			exit.Error = err.Error()
		} else {
			exit.ExitCode = code
		}
	}
	s.log.Info("sandboxd: tty ended", "box", name, "session", tr.Session, "exit_code", exit.ExitCode, "error", exit.Error)
	b, _ := json.Marshal(exit)
	_ = sandbox.WriteFrame(cconn, sandbox.FrameExit, b)
}

// readPid reads the first line ttyArgv's script prints: the tmux client's
// pid inside the box.
func readPid(conn interface{ SetReadDeadline(time.Time) error }, br *bufio.Reader) (int, error) {
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	line, err := br.ReadString('\n')
	if err != nil {
		return 0, fmt.Errorf("reading the client's pid: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid < 2 {
		return 0, fmt.Errorf("the tty printed %q, not a pid", strings.TrimSpace(line))
	}
	return pid, nil
}

// hangUp sends SIGHUP to a tmux client in a box: it detaches, and its
// session keeps running.
func (s *Server) hangUp(id string, pid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e := execSpec(sandbox.AttachRequest{Argv: []string{"kill", "-HUP", strconv.Itoa(pid)}})
	e.AttachStdin = false
	execID, err := s.d.createExec(ctx, id, e)
	if err == nil {
		var conn interface{ Close() error }
		var br *bufio.Reader
		conn, br, err = s.d.startExec(ctx, execID)
		if err == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(br, 64<<10))
			_ = conn.Close()
		}
	}
	if err != nil {
		s.log.Warn("sandboxd: tty hang-up", "id", id, "pid", pid, "err", err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
