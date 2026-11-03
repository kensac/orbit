package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// Terminals in an agent's box (docs/workspace.md, Terminals; docs/api.md,
// Terminals). A terminal is a tmux session in the box, so it outlives the
// socket, a deploy of orbit and the browser. The socket is authenticated by
// a single-use ticket minted with the authentik token, carried in
// Sec-WebSocket-Protocol so it never lands in a URL or an access log.
const (
	// TerminalProtocol is the socket's subprotocol.
	TerminalProtocol = "orbit.terminal.v1"
	// ticketProtocol prefixes the ticket among the offered subprotocols.
	ticketProtocol = "orbit.ticket."
	// ticketTTL is how long a ticket may wait to be used: the proxy mints
	// it and the browser opens the socket at once.
	ticketTTL = 30 * time.Second
	// ticketsPerMinute bounds a person's tickets (docs/workspace.md).
	ticketsPerMinute = 10
	// terminalPing is the server's keepalive: Cloudflare closes a WebSocket
	// idle for 100 s, and the portfolio's client counts 25 s of silence as
	// a dead socket (it pings every 10 s itself).
	terminalPing = 15 * time.Second
	// maxTerminalClients is the sockets one terminal may have on one
	// instance (docs/workspace.md).
	maxTerminalClients = 8
	// writeTimeout drops a client that cannot take output for this long
	// (code 4008); it reattaches and tmux redraws.
	writeTimeout = 30 * time.Second
	// CloseDrain is the close code a draining instance sends: reconnect,
	// and the ready instance takes the socket.
	CloseDrain websocket.StatusCode = 4001
	// CloseSlow is the close code for a client that stopped reading.
	CloseSlow websocket.StatusCode = 4008
	// anyOrigin binds a ticket minted without an Origin to every allowed
	// one: the portfolio's proxy mints server side, without the browser's.
	anyOrigin = "*"
	// claudeCredential is what the claude preset's TTY gets: the backend
	// credential, as a chat sandbox's CLI does.
	claudeCredential = "CLAUDE_CODE_OAUTH_TOKEN" //nolint:gosec // G101: the variable's name, not a value
)

// presetTitles are the presets a terminal create may name, with their
// default titles. TTYLogin is started by the accounts routes only.
var presetTitles = map[string]string{sandbox.TTYShell: "Shell", sandbox.TTYClaude: "Claude Code (direct)"}

func writeCheck(w http.ResponseWriter, status int, check, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "check": check})
}

// DefaultWorkspaceOrigins is ORBIT_WORKSPACE_ORIGINS' default: the
// portfolio, served from both its apex and www.
var DefaultWorkspaceOrigins = []string{"https://kanishksachdev.com", "https://www.kanishksachdev.com"}

func (s *Server) origins() []string {
	if len(s.WorkspaceOrigins) == 0 {
		return DefaultWorkspaceOrigins
	}
	return s.WorkspaceOrigins
}

// boxAgent is the configured agent of that name when it has a box: an agent
// whose chats run in a workstation sandbox, on an orbit that reaches
// sandboxd.
func (s *Server) boxAgent(name string) (*spec.Agent, bool) {
	a, ok := s.Engine.Current().Config.Agents[name]
	return a, ok && s.Boxes != nil && a.Sandbox == spec.SandboxWorkstation
}

func (s *Server) requireWorkspaceGroup(w http.ResponseWriter, r *http.Request) bool {
	if s.inWorkspaceGroup(r) {
		return true
	}
	writeCheck(w, http.StatusForbidden, "group", fmt.Sprintf("changing a workspace needs membership of %q", s.WorkspaceGroup))
	return false
}

func (s *Server) boxClient(w http.ResponseWriter) (*sandbox.Client, bool) {
	c, err := s.Boxes()
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "sandboxd: "+err.Error())
		return nil, false
	}
	return c, true
}

// boxWorkspace is a box workspace (docs/api.md, Finding a workspace): the
// same shape as a snapshot one, with the box's capabilities.
func boxWorkspace(b store.Box, st sandbox.BoxStatus) *workspaceDoc {
	state := st.State
	if state == sandbox.BoxNone {
		// A box that does not exist yet is made by the first terminal.
		state = sandbox.BoxStopped
	}
	caps := boxCapabilities
	caps.Accounts = true
	return &workspaceDoc{
		ID: b.ID, Scope: "agent", Agent: b.Agent, Backend: backendBox, Cwd: "",
		Capabilities: caps,
		Box:          map[string]any{"host": st.Host, "arch": st.Arch, "state": state, "image": st.Image},
		CreatedAt:    b.CreatedAt,
	}
}

// agentWorkspace serves GET /agents/{name}/workspace, one route over both
// backends: an agent with a box gets its box, any other its snapshot
// workspace (that of its default conversation), and an agent with neither
// gets null.
func (s *Server) agentWorkspace(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.Engine.Current().Config.Agents[name]; !ok {
		wsFail(w, store.ErrNotFound)
		return
	}
	if _, ok := s.boxAgent(name); !ok {
		s.snapshotAgentWorkspace(w, r, name)
		return
	}
	c, ok := s.boxClient(w)
	if !ok {
		return
	}
	b, err := s.Store.EnsureBox(r.Context(), name)
	if err != nil {
		respond(w, nil, err)
		return
	}
	st, err := c.Box(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	respond(w, map[string]any{"workspace": boxWorkspace(b, st)}, nil)
}

// workspaceBox resolves {id} to a box whose agent still has one.
func (s *Server) workspaceBox(w http.ResponseWriter, r *http.Request) (store.Box, bool) {
	b, err := s.Store.GetBox(r.Context(), r.PathValue("id"))
	if err != nil {
		writeCheck(w, http.StatusNotFound, "not_found", "no such workspace")
		return b, false
	}
	if _, ok := s.boxAgent(b.Agent); !ok {
		writeCheck(w, http.StatusConflict, "no_shell", "this workspace has no terminals: its agent has no box")
		return b, false
	}
	return b, true
}

func (s *Server) createTerminal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Preset  string          `json:"preset"`
		Cwd     string          `json:"cwd"`
		Cols    int             `json:"cols"`
		Rows    int             `json:"rows"`
		Title   string          `json:"title"`
		Claude  json.RawMessage `json:"claude"`
		Account string          `json:"account"`
	}
	if !decode(w, r, 8<<10, &body, bodyRequired) {
		return
	}
	b, ok := s.workspaceBox(w, r)
	if !ok || !s.requireWorkspaceGroup(w, r) {
		return
	}
	title, known := presetTitles[body.Preset]
	switch {
	case !known:
		writeErr(w, http.StatusBadRequest, "preset: want shell or claude")
		return
	case len(body.Claude) > 0 && string(body.Claude) != "null":
		writeErr(w, http.StatusBadRequest, "claude options (resume, continue, model) are not built yet")
		return
	case !sandbox.ValidCwd(body.Cwd):
		writeCheck(w, http.StatusBadRequest, "path", "cwd: want a clean path inside the workspace")
		return
	case (sandbox.Resize{Cols: body.Cols, Rows: body.Rows}).Validate() != nil:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("cols and rows: want 1 to %d and 1 to %d", sandbox.MaxCols, sandbox.MaxRows))
		return
	case len(body.Title) > 200:
		writeErr(w, http.StatusBadRequest, "title: at most 200 bytes")
		return
	}
	if body.Title != "" {
		title = body.Title
	}
	var acct *store.BoxAccount
	if body.Account != "" {
		a, ok := s.workspaceAccount(w, r, b, body.Account)
		if !ok {
			return
		}
		acct = &a
	}
	c, ok := s.boxClient(w)
	if !ok {
		return
	}
	// The box is made or started here, so a terminal that cannot run says
	// so now rather than on its socket.
	if !s.readyBox(w, r, c, b, acct) {
		return
	}
	s.startTerminal(w, r, b, acct, store.NewBoxTerminal{WorkspaceID: b.ID, Preset: body.Preset, Title: title,
		Cwd: body.Cwd, Cols: body.Cols, Rows: body.Rows, CreatedBy: actor(r, "operator")})
}

// ensureBox makes or starts an agent's box with every account of its
// workspace among its mounts (a running box keeps what it has).
func (s *Server) ensureBox(ctx context.Context, c *sandbox.Client, b store.Box) (sandbox.BoxStatus, error) {
	slots, err := s.Store.AccountSlots(ctx, b.ID)
	if err != nil {
		return sandbox.BoxStatus{}, err
	}
	return c.EnsureBox(ctx, b.Agent, slots)
}

// readyBox ensures the box and, for an account, that the box mounts it and
// its directory links the shared profile, writing an error otherwise.
func (s *Server) readyBox(w http.ResponseWriter, r *http.Request, c *sandbox.Client, b store.Box, acct *store.BoxAccount) bool {
	st, err := s.ensureBox(r.Context(), c, b)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return false
	}
	if acct == nil {
		return true
	}
	if !slices.Contains(st.Accounts, acct.Slot) {
		writeCheck(w, http.StatusConflict, "restart", "the box does not mount this account yet: it mounts new accounts when it restarts")
		return false
	}
	if err := c.PrepareAccount(r.Context(), b.Agent, acct.Slot); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return false
	}
	return true
}

// startTerminal records a terminal and answers 201 with it.
func (s *Server) startTerminal(w http.ResponseWriter, r *http.Request, b store.Box, acct *store.BoxAccount, n store.NewBoxTerminal) {
	if acct != nil {
		n.AccountID = acct.ID
	}
	t, err := s.Store.CreateTerminal(r.Context(), n)
	if errors.Is(err, store.ErrLimit) {
		writeCheck(w, http.StatusConflict, "limit", err.Error())
		return
	}
	if err == nil {
		s.Log.Info("terminal created", "terminal", t.ID, "agent", b.Agent, "preset", t.Preset, "account", n.AccountID, "by", t.CreatedBy)
	}
	respondStatus(w, http.StatusCreated, map[string]any{"terminal": s.withClients(t)}, err)
}

func (s *Server) listTerminals(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	switch state {
	case "", "all":
		state = ""
	case store.TerminalRunning, store.TerminalExited:
	default:
		writeErr(w, http.StatusBadRequest, "state: want running, exited or all")
		return
	}
	b, err := s.Store.GetBox(r.Context(), r.PathValue("id"))
	if err != nil {
		writeCheck(w, http.StatusNotFound, "not_found", "no such workspace")
		return
	}
	ts, err := s.Store.ListTerminals(r.Context(), b.ID, state)
	for i := range ts {
		ts[i] = s.withClients(ts[i])
	}
	respond(w, map[string]any{"terminals": orEmpty(ts)}, err)
}

func (s *Server) terminal(w http.ResponseWriter, r *http.Request) (store.BoxTerminal, bool) {
	t, err := s.Store.GetTerminal(r.Context(), r.PathValue("tid"))
	if errors.Is(err, store.ErrNotFound) {
		writeCheck(w, http.StatusNotFound, "not_found", "no such terminal")
		return t, false
	}
	if err != nil {
		respond(w, nil, err)
		return t, false
	}
	return t, true
}

func (s *Server) getTerminal(w http.ResponseWriter, r *http.Request) {
	if t, ok := s.terminal(w, r); ok {
		respond(w, map[string]any{"terminal": s.withClients(t)}, nil)
	}
}

// killTerminal ends a terminal's tmux session and records it killed. Every
// signal ends the session the same way (tmux kill-session); ending one
// already ended returns it as it is.
func (s *Server) killTerminal(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaceGroup(w, r) {
		return
	}
	var body struct {
		Signal string `json:"signal"`
	}
	if !decode(w, r, 1<<10, &body, bodyOptional) {
		return
	}
	if body.Signal != "" && !slices.Contains([]string{"HUP", "INT", "TERM", "KILL"}, body.Signal) {
		writeErr(w, http.StatusBadRequest, "signal: want HUP, INT, TERM or KILL")
		return
	}
	t, ok := s.terminal(w, r)
	if !ok {
		return
	}
	if t.EndedAt != nil {
		respond(w, map[string]any{"terminal": s.withClients(t)}, nil)
		return
	}
	if s.Boxes != nil {
		if c, err := s.Boxes(); err == nil {
			// A stopped or missing box has no session to end.
			if err := c.KillSession(r.Context(), t.Agent, t.Session); err != nil && !errors.Is(err, sandbox.ErrNoBox) && !isStopped(err) {
				writeErr(w, http.StatusBadGateway, err.Error())
				return
			}
		}
	}
	t, err := s.Store.EndTerminal(r.Context(), t.ID, store.EndKilled, nil)
	if errors.Is(err, store.ErrConflict) {
		t, err = s.Store.GetTerminal(r.Context(), r.PathValue("tid"))
	}
	if err == nil {
		s.Log.Info("terminal killed", "terminal", t.ID, "agent", t.Agent, "by", actor(r, "operator"))
	}
	respond(w, map[string]any{"terminal": s.withClients(t)}, err)
}

func isStopped(err error) bool {
	var se *sandbox.Error
	return errors.As(err, &se) && se.Status == http.StatusConflict
}

// attachTicket mints a socket ticket for the token's person, this terminal
// and an Origin: the body's origin, else the request's Origin header, else
// any of ORBIT_WORKSPACE_ORIGINS (the socket refuses every other Origin
// regardless).
func (s *Server) attachTicket(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaceGroup(w, r) {
		return
	}
	var body struct {
		Origin string `json:"origin"`
	}
	if !decode(w, r, 1<<10, &body, bodyOptional) {
		return
	}
	t, ok := s.terminal(w, r)
	if !ok {
		return
	}
	if t.EndedAt != nil {
		writeCheck(w, http.StatusConflict, "exited", "the terminal has ended")
		return
	}
	origin := body.Origin
	if origin == "" {
		origin = r.Header.Get("Origin")
	}
	if origin == "" {
		origin = anyOrigin
	} else if !slices.Contains(s.origins(), origin) {
		writeCheck(w, http.StatusBadRequest, "origin", "origin "+origin+" is not in ORBIT_WORKSPACE_ORIGINS")
		return
	}
	person := actor(r, "operator")
	n, err := s.Store.TicketsSince(r.Context(), person, time.Now().Add(-time.Minute))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if n >= ticketsPerMinute {
		w.Header().Set("Retry-After", "60")
		writeCheck(w, http.StatusTooManyRequests, "rate", fmt.Sprintf("at most %d attach tickets a minute", ticketsPerMinute))
		return
	}
	ticket, exp, err := s.Store.MintTicket(r.Context(), t.ID, person, origin, ticketTTL)
	if err != nil {
		respond(w, nil, err)
		return
	}
	base := strings.TrimRight(s.Engine.Current().Config.Deployment.PublicURL, "/")
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	respond(w, map[string]any{
		"url":        base + "/api/v1/terminals/" + t.ID + "/ws?ticket=" + ticket,
		"protocol":   TerminalProtocol,
		"ticket":     ticket,
		"protocols":  []string{TerminalProtocol, ticketProtocol + ticket},
		"expires_at": exp,
	}, nil)
}

// withClients fills a terminal's attached-client count on this instance.
func (s *Server) withClients(t store.BoxTerminal) store.BoxTerminal {
	s.termMu.Lock()
	t.Clients = s.termClients[t.ID]
	s.termMu.Unlock()
	return t
}

func (s *Server) addClient(id string) bool {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	if s.termClients == nil {
		s.termClients = map[string]int{}
	}
	if s.termClients[id] >= maxTerminalClients {
		return false
	}
	s.termClients[id]++
	return true
}

func (s *Server) dropClient(id string) {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	if s.termClients[id]--; s.termClients[id] <= 0 {
		delete(s.termClients, id)
	}
}

// offeredTicket finds the ticket among the offered subprotocols, else in
// the query (a client that cannot set subprotocols), and requires the
// terminal protocol to be offered.
func offeredTicket(r *http.Request) (ticket string, ok bool) {
	proto := false
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(h, ",") {
			p = strings.TrimSpace(p)
			if p == TerminalProtocol {
				proto = true
			} else if t, found := strings.CutPrefix(p, ticketProtocol); found {
				ticket = t
			}
		}
	}
	if ticket == "" {
		ticket = r.URL.Query().Get("ticket")
	}
	return ticket, proto && ticket != ""
}

type control struct {
	Type string `json:"type"`
	Cols int    `json:"cols,omitempty"`
	Rows int    `json:"rows,omitempty"`
}

// terminalSocket is GET /terminals/{tid}/ws: binary frames are the
// terminal's bytes both ways, text frames JSON control (docs/api.md,
// Terminals).
func (s *Server) terminalSocket(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !slices.Contains(s.origins(), origin) {
		// A cross-site page must never open a terminal, ticket or not.
		writeCheck(w, http.StatusForbidden, "origin", "origin not allowed")
		return
	}
	ticket, ok := offeredTicket(r)
	if !ok {
		unauthorized(w, "ticket", "offer the subprotocols "+TerminalProtocol+" and "+ticketProtocol+"<ticket>")
		return
	}
	tid := r.PathValue("tid")
	person, err := s.Store.RedeemTicket(r.Context(), ticket, tid, origin, anyOrigin)
	if errors.Is(err, store.ErrNotFound) {
		unauthorized(w, "ticket", "the ticket is unknown, used, expired, or for another terminal or origin")
		return
	}
	if err != nil {
		respond(w, nil, err)
		return
	}
	// The server's WriteTimeout would otherwise cut the socket: a hijacked
	// connection keeps the deadlines the request was served under.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	_ = rc.SetReadDeadline(time.Time{})
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{TerminalProtocol},
		// Origin is checked above against ORBIT_WORKSPACE_ORIGINS, which
		// is not the request's own host.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	fail := func(check, msg string) {
		b, _ := json.Marshal(map[string]string{"type": "error", "check": check, "error": msg})
		_ = conn.Write(ctx, websocket.MessageText, b)
		_ = conn.Close(websocket.StatusPolicyViolation, check)
	}
	t, err := s.Store.GetTerminal(ctx, tid)
	if err != nil {
		fail("not_found", "no such terminal")
		return
	}
	if t.EndedAt != nil {
		fail("exited", "the terminal has ended")
		return
	}
	if _, ok := s.boxAgent(t.Agent); !ok {
		fail("no_shell", "this terminal's agent has no box")
		return
	}
	c, err := s.Boxes()
	if err != nil {
		fail("box", err.Error())
		return
	}
	if !s.addClient(t.ID) {
		fail("limit", fmt.Sprintf("at most %d clients per terminal", maxTerminalClients))
		return
	}
	defer s.dropClient(t.ID)
	if _, err := s.ensureBox(ctx, c, store.Box{ID: t.WorkspaceID, Agent: t.Agent}); err != nil {
		fail("box", err.Error())
		return
	}
	tr := sandbox.TTYRequest{Preset: t.Preset, Session: t.Session, Cwd: t.Cwd, Cols: t.Cols, Rows: t.Rows, Account: t.AccountSlot}
	if t.MCPServer != nil {
		tr.MCPServer = *t.MCPServer
	}
	// An account's terminal uses that account's own login: it is given no
	// credential (sandboxd also removes the token from its session).
	if t.Preset == sandbox.TTYClaude && t.AccountSlot == "" {
		if cred, _ := s.Engine.Env(claudeCredential); cred != "" {
			tr.Env = map[string]string{claudeCredential: cred}
		}
	}
	tty, err := c.AttachTTY(ctx, t.Agent, tr)
	if err != nil {
		fail("box", err.Error())
		return
	}
	defer func() { _ = tty.Close() }()
	_ = s.Store.TerminalAttached(ctx, t.ID, t.Cols, t.Rows)
	s.Log.Info("terminal attached", "terminal", t.ID, "agent", t.Agent, "person", person)
	defer s.Log.Info("terminal detached", "terminal", t.ID, "agent", t.Agent, "person", person)

	write := func(typ websocket.MessageType, b []byte) error {
		wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
		defer wcancel()
		return conn.Write(wctx, typ, b)
	}
	writeJSON := func(v any) error {
		b, _ := json.Marshal(v)
		return write(websocket.MessageText, b)
	}
	t = s.withClients(t)
	if err := writeJSON(map[string]any{"type": "ready", "terminal": t, "replay_bytes": 0}); err != nil {
		return
	}

	// The client: input, resizes and pings, until it closes.
	clientGone := make(chan struct{})
	go func() {
		defer close(clientGone)
		for {
			typ, b, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				if _, err := tty.Write(b); err != nil {
					return
				}
				continue
			}
			var m control
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			switch m.Type {
			case "resize":
				if err := tty.Resize(m.Cols, m.Rows); err == nil {
					_ = s.Store.TerminalAttached(ctx, t.ID, m.Cols, m.Rows)
				}
			case "ping":
				_ = writeJSON(map[string]string{"type": "pong"})
			}
		}
	}()

	// The terminal's output, until its process ends.
	outDone := make(chan error, 1)
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := tty.Read(buf)
			if n > 0 {
				if werr := write(websocket.MessageBinary, buf[:n]); werr != nil {
					outDone <- werr
					return
				}
			}
			if err != nil {
				outDone <- err
				return
			}
		}
	}()

	ping := s.TerminalPing
	if ping <= 0 {
		ping = terminalPing
	}
	tick := time.NewTicker(ping)
	defer tick.Stop()
	var drain <-chan struct{}
	if s.Drain != nil {
		drain = s.Drain.Done()
	}
	for {
		select {
		case <-clientGone:
			return
		case <-tick.C:
			if err := writeJSON(map[string]string{"type": "ping"}); err != nil {
				_ = conn.Close(CloseSlow, "not reading")
				return
			}
		case <-drain:
			_ = conn.Close(CloseDrain, "orbit is restarting: reconnect")
			return
		case <-s.Stop:
			_ = conn.Close(CloseDrain, "orbit is restarting: reconnect")
			return
		case err := <-outDone:
			if tty.Exit == nil {
				// A write to the client failed, or sandboxd went away.
				if errors.Is(err, context.DeadlineExceeded) {
					_ = conn.Close(CloseSlow, "not reading")
				} else {
					fail("box", err.Error())
				}
				return
			}
			s.ended(ctx, c, t, *tty.Exit, writeJSON)
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		}
	}
}

// ended handles the tmux client's exit: when its session is gone too the
// terminal has exited and is recorded so; when the session lives (a tmux
// detach) the socket just closes.
func (s *Server) ended(ctx context.Context, c *sandbox.Client, t store.BoxTerminal, ex sandbox.Exit, writeJSON func(any) error) {
	alive, err := c.SessionExists(ctx, t.Agent, t.Session)
	if err != nil || alive {
		_ = writeJSON(map[string]any{"type": "detached"})
		return
	}
	code := ex.ExitCode
	if _, err := s.Store.EndTerminal(ctx, t.ID, store.EndExit, &code); err != nil && !errors.Is(err, store.ErrConflict) {
		s.Log.Warn("terminal: record exit", "terminal", t.ID, "err", err)
	}
	if t.AccountID != nil {
		// A login just ended: what the account's files now say is read
		// before the client hears of the exit and lists its accounts.
		if a, err := s.Store.GetAccount(ctx, *t.AccountID); err == nil {
			_ = s.refreshAccount(ctx, c, a)
		}
	}
	_ = writeJSON(map[string]any{"type": "exit", "code": code, "reason": store.EndExit})
}
