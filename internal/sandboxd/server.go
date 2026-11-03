package sandboxd

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kensac/orbit/internal/credproxy"
	"github.com/kensac/orbit/internal/sandbox"
)

// Server is sandboxd.
type Server struct {
	cfg Config
	d   *Docker
	// sealer turns a backend credential into a credproxy token; nil (a key Validate would refuse) refuses every attach that carries one.
	sealer *credproxy.Sealer
	log    *slog.Logger
	now    func() time.Time
	// mu serializes creates, so MaxSandboxes holds between the count and
	// the create.
	mu sync.Mutex
	// ready caches the last readiness answer (health.go).
	readyMu sync.Mutex
	ready   sandbox.Readiness
	readyAt time.Time
	// boxMu serializes box creates, starts and stops.
	boxMu sync.Mutex
	// claudeConfigMu serializes config imports with each other.
	claudeConfigMu sync.Mutex
	// host and arch are the docker host's, from Check, for BoxStatus.
	host, arch string
}

// New returns a server for cfg, which must have passed Validate.
func New(cfg Config, log *slog.Logger) *Server {
	sealer, _ := credproxy.NewSealer(cfg.CredProxyKey)
	return &Server{cfg: cfg, d: NewDocker(cfg.Docker), log: log, now: time.Now, sealer: sealer}
}

// Check verifies the host before serving: the daemon has seccomp on, the
// configured runtime exists, the network is internal (no route out), and
// the image is present (pulled if not).
func (s *Server) Check(ctx context.Context) error {
	info, err := s.d.info(ctx)
	if err != nil {
		return err
	}
	seccomp := false
	for _, o := range info.SecurityOptions {
		if strings.HasPrefix(o, "name=seccomp") {
			seccomp = true
		}
		if strings.HasPrefix(o, "name=userns") {
			s.log.Info("docker remaps user namespaces")
		}
	}
	if !seccomp {
		return errors.New("docker has no seccomp profile; refusing to run build containers")
	}
	s.host, s.arch = info.Name, info.Architecture
	for _, rt := range []string{s.cfg.Runtime, s.cfg.BoxRuntime} {
		if rt == "" {
			continue
		}
		if _, ok := info.Runtimes[rt]; !ok {
			return fmt.Errorf("docker has no runtime %q", rt)
		}
	}
	nets := []string{s.cfg.Network}
	if s.cfg.WorkstationNetwork != "" {
		nets = append(nets, s.cfg.WorkstationNetwork)
	}
	for _, name := range nets {
		n, err := s.d.network(ctx, name)
		if err != nil {
			return fmt.Errorf("network %s: %w", name, err)
		}
		if !n.Internal {
			return fmt.Errorf("network %s is not internal: a sandbox on it could reach anything; create it with internal: true", name)
		}
	}
	for _, img := range []string{s.cfg.Image, s.cfg.BoxImage} {
		if img == "" {
			continue
		}
		ok, err := s.d.imageExists(ctx, img)
		if err != nil {
			return err
		}
		if !ok {
			s.log.Info("pulling image", "image", img)
			if err := s.d.pull(ctx, img); err != nil {
				return err
			}
		}
	}
	return nil
}

// Handler serves the sandbox and box operations, each behind the token, and the two probes without it (health.go).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+sandbox.PathSandboxes, s.create)
	mux.HandleFunc("POST "+sandbox.PathSandboxes+"/{id}/attach", s.attach)
	mux.HandleFunc("GET "+sandbox.PathSandboxes+"/{id}/archive", s.archive)
	mux.HandleFunc("DELETE "+sandbox.PathSandboxes+"/{id}", s.destroy)
	mux.HandleFunc("GET "+sandbox.PathBoxes+"/{name}", s.boxGet)
	mux.HandleFunc("POST "+sandbox.PathBoxes+"/{name}", s.boxEnsure)
	mux.HandleFunc("POST "+sandbox.PathBoxes+"/{name}/stop", s.boxStop)
	mux.HandleFunc("POST "+sandbox.PathBoxes+"/{name}/recreate", s.boxRecreate)
	mux.HandleFunc("POST "+sandbox.PathBoxes+"/{name}/accounts/{slot}/prepare", s.accountPrepare)
	mux.HandleFunc("GET "+sandbox.PathBoxes+"/{name}/accounts/{slot}", s.accountStatus)
	mux.HandleFunc("GET "+sandbox.PathBoxes+"/{name}/accounts/{slot}/claude-config", s.claudeConfigGet)
	mux.HandleFunc("PUT "+sandbox.PathBoxes+"/{name}/accounts/{slot}/claude-config", s.claudeConfigPut)
	mux.HandleFunc("POST "+sandbox.PathBoxes+"/{name}/tty", s.tty)
	mux.HandleFunc("GET "+sandbox.PathBoxes+"/{name}/sessions/{session}", s.sessionExists)
	mux.HandleFunc("POST "+sandbox.PathBoxes+"/{name}/sessions/{session}/kill", s.sessionKill)
	authed := s.auth(mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case sandbox.PathHealthz:
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, "ok\n")
				return
			case sandbox.PathReadyz:
				s.readyz(w, r)
				return
			}
		}
		authed.ServeHTTP(w, r)
	})
}

func (s *Server) auth(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			s.log.Warn("sandboxd: unauthorized request", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// errNotManaged is any id that is not a sandbox sandboxd made, including
// every other container on the host: they are indistinguishable from
// missing to a caller.
var errNotManaged = errors.New("no such sandbox")

func (s *Server) managed(ctx context.Context, id string) (containerState, error) {
	if !sandbox.ValidID(id) {
		return containerState{}, errNotManaged
	}
	c, err := s.d.inspect(ctx, id)
	if errors.Is(err, errNotFound) {
		return containerState{}, errNotManaged
	}
	if err != nil {
		return containerState{}, err
	}
	if c.ID != id || c.Config.Labels[LabelManaged] != "1" {
		return containerState{}, errNotManaged
	}
	return c, nil
}

func (s *Server) managedOrError(w http.ResponseWriter, r *http.Request) (containerState, bool) {
	c, err := s.managed(r.Context(), r.PathValue("id"))
	if errors.Is(err, errNotManaged) {
		writeError(w, http.StatusNotFound, err.Error())
		return c, false
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return c, false
	}
	return c, true
}

// profile is the configuration a create's profile runs under: the same
// container options, and for a workstation its own egress proxy.
func (s *Server) profile(name string) (Config, error) {
	cfg := s.cfg
	switch name {
	case "", sandbox.ProfileBuild:
	case sandbox.ProfileWorkstation:
		if cfg.WorkstationProxy == "" {
			return cfg, errors.New("the workstation profile is not configured here (ORBIT_SANDBOX_WORKSTATION_PROXY)")
		}
		cfg.Proxy, cfg.Network = cfg.WorkstationProxy, cfg.WorkstationNetwork
		if cfg.WorkstationCredProxy != "" {
			cfg.CredProxy = cfg.WorkstationCredProxy
		}
	default:
		return cfg, fmt.Errorf("unknown profile %q", name)
	}
	return cfg, nil
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	runID := r.URL.Query().Get("run_id")
	if !sandbox.ValidRunID(runID) {
		writeError(w, http.StatusBadRequest, "run_id must be 1 to 64 letters, digits or dashes")
		return
	}
	cfg, err := s.profile(r.URL.Query().Get("profile"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	now := s.now()
	spec := containerSpec(cfg, runID, now)

	s.mu.Lock()
	existing, err := s.d.managed(ctx)
	if err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if len(existing) >= MaxSandboxes {
		s.mu.Unlock()
		writeError(w, http.StatusTooManyRequests, fmt.Sprintf("%d sandboxes exist, the most allowed", len(existing)))
		return
	}
	id, err := s.d.createContainer(ctx, spec)
	s.mu.Unlock()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	fail := func(status int, msg string) {
		// A context of its own: the request's may be what failed.
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.d.remove(rctx, id); err != nil {
			s.log.Error("sandboxd: remove after a failed create", "id", id, "err", err)
		}
		s.log.Warn("sandboxd: create failed", "run_id", runID, "err", msg)
		writeError(w, status, msg)
	}
	c, err := s.d.inspect(ctx, id)
	if err != nil {
		fail(http.StatusBadGateway, err.Error())
		return
	}
	if err := verifyCreated(cfg, c); err != nil {
		fail(http.StatusInternalServerError, "the daemon did not create the container asked for: "+err.Error())
		return
	}

	body := http.MaxBytesReader(w, r.Body, sandbox.MaxArchiveBytes+(16<<20))
	pr, pw := io.Pipe()
	sanErr := make(chan error, 1)
	go func() {
		err := SanitizeIn(pw, body)
		_ = pw.CloseWithError(err)
		sanErr <- err
	}()
	putErr := s.d.putArchive(ctx, id, sandbox.WorkDir, pr)
	_ = pr.CloseWithError(errors.New("upload ended"))
	if err := <-sanErr; err != nil {
		fail(http.StatusBadRequest, "archive: "+err.Error())
		return
	}
	if putErr != nil {
		fail(http.StatusBadGateway, putErr.Error())
		return
	}
	if err := s.d.start(ctx, id); err != nil {
		fail(http.StatusBadGateway, err.Error())
		return
	}
	expires := now.Add(Lifetime).UTC().Truncate(time.Second)
	s.log.Info("sandboxd: created", "id", id, "run_id", runID, "expires", expires)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(sandbox.CreateResponse{ID: id, ExpiresAt: expires})
}

func (s *Server) destroy(w http.ResponseWriter, r *http.Request) {
	c, ok := s.managedOrError(w, r)
	if !ok {
		return
	}
	if err := s.d.remove(r.Context(), c.ID); err != nil && !errors.Is(err, errNotFound) {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.log.Info("sandboxd: destroyed", "id", c.ID, "run_id", c.Config.Labels[LabelRunID])
	w.WriteHeader(http.StatusNoContent)
}

// archive spools the sanitized workdir to a temporary file first, so a
// refusal (a FIFO, too large) is a clean error status rather than a tar cut
// off mid-stream.
func (s *Server) archive(w http.ResponseWriter, r *http.Request) {
	c, ok := s.managedOrError(w, r)
	if !ok {
		return
	}
	rc, err := s.d.getArchive(r.Context(), c.ID, sandbox.WorkDir)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer rc.Close()
	f, err := os.CreateTemp("", "sandbox-archive-*.tar")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer f.Close()
	// Docker's tar is bounded too: headers and padding over the content cap.
	if err := SanitizeOut(f, io.LimitReader(rc, sandbox.MaxArchiveBytes+(64<<20))); err != nil {
		s.log.Warn("sandboxd: readback refused", "id", c.ID, "err", err)
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err == nil {
		_, err = f.Seek(0, io.SeekStart)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

func (s *Server) attach(w http.ResponseWriter, r *http.Request) {
	c, ok := s.managedOrError(w, r)
	if !ok {
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), sandbox.AttachUpgrade) {
		writeError(w, http.StatusUpgradeRequired, "attach upgrades to "+sandbox.AttachUpgrade)
		return
	}
	if !c.State.Running {
		writeError(w, http.StatusConflict, "sandbox is not running (expired or destroyed)")
		return
	}
	var ar sandbox.AttachRequest
	body := http.MaxBytesReader(w, r.Body, sandbox.MaxArgvBytes+(1<<20))
	dec := json.NewDecoder(body)
	// A field sandboxd does not know (a user, privileges, mounts) is refused,
	// not ignored, so a caller can never believe it set one.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ar); err != nil {
		writeError(w, http.StatusBadRequest, "attach request: "+err.Error())
		return
	}
	if err := ar.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	env, err := s.sealCredentials(c, ar.Env)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ar.Env = env
	// The body is fully read before the connection is hijacked, so what
	// follows on it is the process's stdin and nothing else.
	if _, err := io.Copy(io.Discard, body); err != nil {
		writeError(w, http.StatusBadRequest, "attach request: "+err.Error())
		return
	}
	ctx := r.Context()
	execID, err := s.d.createExec(ctx, c.ID, execSpec(ar))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	dconn, dbr, err := s.d.startExec(ctx, execID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer func() { _ = dconn.Close() }()
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
	_, err = cbuf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + sandbox.AttachUpgrade + "\r\n\r\n")
	if err == nil {
		err = cbuf.Flush()
	}
	if err != nil {
		return
	}
	start := s.now()
	runID := c.Config.Labels[LabelRunID]
	s.log.Info("sandboxd: attached", "id", c.ID, "run_id", runID, "exec", execID, "cmd", ar.Argv[0], "args", len(ar.Argv))

	go func() {
		_, _ = io.Copy(dconn, cbuf.Reader)
		if cw, ok := dconn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	exit := sandbox.Exit{ExitCode: -1}
	if err := demux(dbr, func(typ byte, p []byte) error { return sandbox.WriteFrame(cconn, typ, p) }); err != nil {
		var we writeErr
		if errors.As(err, &we) {
			// orbit went away. The process keeps running until the
			// sandbox's lifetime or a destroy ends it.
			s.log.Warn("sandboxd: attach client gone", "id", c.ID, "run_id", runID, "err", err)
			return
		}
		exit.Error = "process stream: " + err.Error()
	}
	if exit.Error == "" {
		code, err := s.exitCode(execID)
		if err != nil {
			exit.Error = err.Error()
		} else {
			exit.ExitCode = code
		}
	}
	s.log.Info("sandboxd: process exited", "id", c.ID, "run_id", runID, "exec", execID, "exit_code", exit.ExitCode, "error", exit.Error, "duration_ms", s.now().Sub(start).Milliseconds())
	b, _ := json.Marshal(exit)
	_ = sandbox.WriteFrame(cconn, sandbox.FrameExit, b)
}

// exitCode waits briefly for docker to record the exec's exit: the stream
// can close a moment before the state does.
func (s *Server) exitCode(execID string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		st, err := s.d.inspectExec(ctx, execID)
		if err != nil {
			return -1, err
		}
		if !st.Running {
			return st.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return -1, errors.New("process stream closed but the process is still running")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

type writeErr struct{ error }

func (e writeErr) Unwrap() error { return e.error }

// demux reads docker's multiplexed stream (8-byte headers: stream type,
// three zero bytes, big-endian length) and hands stdout and stderr on in
// chunks. Docker's system-error stream is shown as stderr. io.EOF between
// frames is the process's end.
func demux(r io.Reader, emit func(typ byte, p []byte) error) error {
	buf := make([]byte, 32<<10)
	var h [8]byte
	for {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var typ byte
		switch h[0] {
		case 1:
			typ = sandbox.FrameStdout
		case 2, 3:
			typ = sandbox.FrameStderr
		default:
			return fmt.Errorf("unexpected docker stream %d", h[0])
		}
		n := int64(binary.BigEndian.Uint32(h[4:]))
		for n > 0 {
			k := min(n, int64(len(buf)))
			if _, err := io.ReadFull(r, buf[:k]); err != nil {
				return err
			}
			if err := emit(typ, buf[:k]); err != nil {
				return writeErr{err}
			}
			n -= k
		}
	}
}

// Reap removes sandboxes past their lifetime and grace, and sandboxd's own
// volumes no container uses (a crash between create and remove). Only what
// carries LabelManaged is ever touched.
func (s *Server) Reap(ctx context.Context) {
	cs, err := s.d.managed(ctx)
	if err != nil {
		s.log.Error("sandboxd: reap: list", "err", err)
		return
	}
	now := s.now()
	for _, c := range cs {
		if c.Labels[LabelManaged] != "1" {
			continue
		}
		exp, err := strconv.ParseInt(c.Labels[LabelExpires], 10, 64)
		if err == nil && now.Before(time.Unix(exp, 0).Add(Grace)) {
			continue
		}
		if err := s.d.remove(ctx, c.ID); err != nil && !errors.Is(err, errNotFound) {
			s.log.Error("sandboxd: reap", "id", c.ID, "err", err)
			continue
		}
		s.log.Info("sandboxd: reaped", "id", c.ID, "run_id", c.Labels[LabelRunID])
	}
	vols, err := s.d.danglingVolumes(ctx)
	if err != nil {
		s.log.Error("sandboxd: reap: volumes", "err", err)
		return
	}
	for _, v := range vols {
		if err := s.d.removeVolume(ctx, v); err != nil && !errors.Is(err, errNotFound) {
			s.log.Error("sandboxd: reap volume", "volume", v, "err", err)
			continue
		}
		s.log.Info("sandboxd: reaped volume", "volume", v)
	}
}

// reapEvery is how often the reaper runs: far inside Grace.
const reapEvery = time.Minute

// Run checks the host, then serves on cfg.Listen and reaps until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Check(ctx); err != nil {
		return err
	}
	ln, err := listen(s.cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		t := time.NewTicker(reapEvery)
		defer t.Stop()
		for {
			s.Reap(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	go func() { //nolint:gosec // shutdown runs after ctx has ended, so it cannot use ctx
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	s.log.Info("sandboxd: serving", "listen", s.cfg.Listen, "image", s.cfg.Image, "network", s.cfg.Network, "credproxy", s.cfg.CredProxy, "box_image", s.cfg.BoxImage, "box_runtime", s.cfg.BoxRuntime)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func listen(addr string) (net.Listener, error) {
	if p, ok := strings.CutPrefix(addr, "unix:"); ok {
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(p)
		}
		ln, err := net.Listen("unix", p)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(p, 0o600); err != nil {
			_ = ln.Close()
			return nil, err
		}
		return ln, nil
	}
	return net.Listen("tcp", addr)
}
