package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/kensac/orbit/internal/credproxy"
	"github.com/kensac/orbit/internal/sandbox"
)

// LabelChatBox marks one persistent container dedicated to a single chat
// conversation. It has no direct terminal or account-volume routes.
const LabelChatBox = "orbit.chatbox"

func chatBoxSpec(cfg Config, name string) containerCreate {
	c := boxSpec(cfg, name, nil)
	c.Labels[LabelChatBox] = name
	return c
}

func (s *Server) chatBoxState(ctx context.Context, name string) (containerState, error) {
	if !sandbox.ValidChatBoxName(name) {
		return containerState{}, fmt.Errorf("invalid chat box name %q", name)
	}
	c, err := s.d.inspect(ctx, boxContainer(name))
	if err != nil {
		return c, err
	}
	if c.Config.Labels[LabelChatBox] != name || len(mountedAccounts(c)) != 0 {
		return c, fmt.Errorf("container %s is not an isolated chat box", boxContainer(name))
	}
	if err := verifyBox(s.cfg, name, c); err != nil {
		return c, fmt.Errorf("container %s is not this chat box: %w", boxContainer(name), err)
	}
	return c, nil
}

func (s *Server) chatBoxEnsure(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sandbox.ValidChatBoxName(name) {
		writeError(w, http.StatusBadRequest, "invalid chat box name")
		return
	}
	if err := s.boxesOn(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	req, ok := ensureRequest(w, r)
	if !ok {
		return
	}
	if len(req.Accounts) != 0 {
		writeError(w, http.StatusBadRequest, "a chat box does not mount Claude accounts")
		return
	}
	ctx := r.Context()
	s.boxMu.Lock()
	defer s.boxMu.Unlock()
	c, err := s.chatBoxState(ctx, name)
	switch {
	case errors.Is(err, errNotFound):
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
		return
	case c.State.Running:
		writeJSON(w, http.StatusOK, s.status(name, &c))
		return
	case c.Config.Image != s.cfg.BoxImage:
		if err := s.d.remove(ctx, c.ID); err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		err = errNotFound
	}
	if errors.Is(err, errNotFound) {
		id, err := s.d.createNamed(ctx, boxContainer(name), chatBoxSpec(s.cfg, name))
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}
		if c, err = s.chatBoxState(ctx, name); err != nil {
			if rerr := s.d.remove(context.WithoutCancel(ctx), id); rerr != nil {
				s.log.Error("sandboxd: remove after a failed chat box create", "name", name, "err", rerr)
			}
			writeError(w, http.StatusInternalServerError, "the daemon did not create the chat box asked for: "+err.Error())
			return
		}
		s.log.Info("sandboxd: chat box created", "name", name, "id", id, "image", s.cfg.BoxImage)
	}
	if err := s.d.start(ctx, c.ID); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	c.State.Running = true
	writeJSON(w, http.StatusOK, s.status(name, &c))
}

func (s *Server) chatBoxArchive(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, err := s.chatBoxState(r.Context(), name)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, sandbox.ErrNoBox.Error())
		return
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
		return
	case !c.State.Running:
		writeError(w, http.StatusConflict, "the chat box is stopped")
		return
	}
	rc, err := s.d.getArchive(r.Context(), c.ID, sandbox.WorkDir)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer rc.Close()
	f, err := os.CreateTemp("", "orbit-chatbox-archive-*.tar")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer f.Close()
	if err := SanitizeOut(f, io.LimitReader(rc, sandbox.MaxArchiveBytes+(64<<20))); err != nil {
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

func (s *Server) sealChatBoxCredentials(c containerState, runID string, env map[string]string) (map[string]string, error) {
	if err := (sandbox.ChatBoxAttachRequest{RunID: runID, Exec: sandbox.AttachRequest{Argv: []string{"orbit"}, Env: env}}).Validate(); err != nil {
		return nil, err
	}
	if len(env) == 0 {
		return map[string]string{}, nil
	}
	if s.sealer == nil {
		return nil, errors.New("no credential proxy key: refusing to hand a chat box the backend credential")
	}
	network := c.HostConfig.NetworkMode
	n, ok := c.NetworkSettings.Networks[network]
	if !ok {
		return nil, fmt.Errorf("chat box is not on %s", network)
	}
	addr, err := netip.ParseAddr(n.IPAddress)
	if err != nil {
		return nil, fmt.Errorf("chat box has no address on %s", network)
	}
	out := make(map[string]string, len(env))
	expires := s.now().Add(sandbox.ChatBoxCredentialTTL)
	for kind, credential := range env {
		tok, err := s.sealer.Seal(credproxy.Grant{Kind: kind, Credential: credential, Addr: addr, Expires: expires,
			Sandbox: c.ID, RunID: runID})
		if err != nil {
			return nil, err
		}
		out[kind] = tok
	}
	return out, nil
}

func (s *Server) chatBoxAttach(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, err := s.chatBoxState(r.Context(), name)
	switch {
	case errors.Is(err, errNotFound):
		writeError(w, http.StatusNotFound, sandbox.ErrNoBox.Error())
		return
	case err != nil:
		writeError(w, http.StatusConflict, err.Error())
		return
	case !c.State.Running:
		writeError(w, http.StatusConflict, "the chat box is stopped")
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), sandbox.AttachUpgrade) {
		writeError(w, http.StatusUpgradeRequired, "attach upgrades to "+sandbox.AttachUpgrade)
		return
	}
	var ar sandbox.ChatBoxAttachRequest
	body := http.MaxBytesReader(w, r.Body, sandbox.MaxArgvBytes+(1<<20))
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ar); err != nil {
		writeError(w, http.StatusBadRequest, "chat box attach request: "+err.Error())
		return
	}
	if err := ar.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		writeError(w, http.StatusBadRequest, "chat box attach request: "+err.Error())
		return
	}
	env, err := s.sealChatBoxCredentials(c, ar.RunID, ar.Exec.Env)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ar.Exec.Env = env
	e := execSpec(ar.Exec)
	if len(env) > 0 {
		e.Env = append(e.Env, "ANTHROPIC_BASE_URL=http://"+s.cfg.WorkstationCredProxy)
		host, _, err := net.SplitHostPort(s.cfg.WorkstationCredProxy)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "invalid workstation credential proxy address")
			return
		}
		e.Env = append(e.Env, "NO_PROXY="+host, "no_proxy="+host)
	}
	ctx := r.Context()
	execID, err := s.d.createExec(ctx, c.ID, e)
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
	started := s.now()
	s.log.Info("sandboxd: chat box process attached", "name", name, "run_id", ar.RunID, "exec", execID, "cmd", ar.Exec.Argv[0], "args", len(ar.Exec.Argv))
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
			s.log.Warn("sandboxd: chat box client gone", "name", name, "run_id", ar.RunID, "err", err)
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
	s.log.Info("sandboxd: chat box process exited", "name", name, "run_id", ar.RunID, "exit_code", exit.ExitCode, "error", exit.Error, "duration_ms", s.now().Sub(started).Milliseconds())
	b, _ := json.Marshal(exit)
	_ = sandbox.WriteFrame(cconn, sandbox.FrameExit, b)
}
