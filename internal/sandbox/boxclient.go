package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

func (c *Client) boxCall(ctx context.Context, method, p string, out any) error {
	return c.boxSend(ctx, method, p, nil, out)
}

// boxSend is boxCall with a JSON body.
func (c *Client) boxSend(ctx context.Context, method, p string, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, PathBoxes+p, body, ctype)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoBox
	}
	if resp.StatusCode >= 300 {
		return readError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
}

func boxPath(name string, rest ...string) (string, error) {
	if !ValidBoxName(name) {
		return "", fmt.Errorf("invalid box name %q", name)
	}
	p := "/" + name
	for _, r := range rest {
		p += "/" + url.PathEscape(r)
	}
	return p, nil
}

// EnsureBox creates the agent's box, or starts it when it is stopped, and
// returns it running. accounts are the account slots it mounts: a box that
// is made or recreated mounts them (and any it mounted already); a running
// one is returned as it is, its Accounts saying what it mounts.
func (c *Client) EnsureBox(ctx context.Context, name string, accounts []string) (BoxStatus, error) {
	return c.ensure(ctx, name, "", accounts)
}

// RecreateBox replaces the box's container with one that mounts accounts,
// and starts it. Its volumes stay; its processes, every tmux session
// among them, end.
func (c *Client) RecreateBox(ctx context.Context, name string, accounts []string) (BoxStatus, error) {
	return c.ensure(ctx, name, "recreate", accounts)
}

func (c *Client) ensure(ctx context.Context, name, op string, accounts []string) (BoxStatus, error) {
	var st BoxStatus
	req := EnsureRequest{Accounts: accounts}
	if err := req.Validate(); err != nil {
		return st, err
	}
	var rest []string
	if op != "" {
		rest = append(rest, op)
	}
	p, err := boxPath(name, rest...)
	if err == nil {
		err = c.boxSend(ctx, http.MethodPost, p, req, &st)
	}
	return st, err
}

// PrepareAccount makes an account's directory the box user's and links the
// shared profile (settings, CLAUDE.md, skills, commands, agents) into it.
// It is safe to repeat.
func (c *Client) PrepareAccount(ctx context.Context, name, slot string) error {
	if !ValidSlot(slot) {
		return fmt.Errorf("invalid account slot %q", slot)
	}
	p, err := boxPath(name, "accounts", slot, "prepare")
	if err != nil {
		return err
	}
	return c.boxCall(ctx, http.MethodPost, p, nil)
}

// AccountStatus reads an account's login from its files in the box.
func (c *Client) AccountStatus(ctx context.Context, name, slot string) (AccountStatus, error) {
	var st AccountStatus
	if !ValidSlot(slot) {
		return st, fmt.Errorf("invalid account slot %q", slot)
	}
	p, err := boxPath(name, "accounts", slot)
	if err == nil {
		err = c.boxCall(ctx, http.MethodGet, p, &st)
	}
	return st, err
}

// Box reports a box; its State is BoxNone when there is none.
func (c *Client) Box(ctx context.Context, name string) (BoxStatus, error) {
	var st BoxStatus
	p, err := boxPath(name)
	if err == nil {
		err = c.boxCall(ctx, http.MethodGet, p, &st)
	}
	return st, err
}

// StopBox stops a box; its volumes stay.
func (c *Client) StopBox(ctx context.Context, name string) (BoxStatus, error) {
	var st BoxStatus
	p, err := boxPath(name, "stop")
	if err == nil {
		err = c.boxCall(ctx, http.MethodPost, p, &st)
	}
	return st, err
}

// SessionExists reports whether a tmux session is alive in a running box.
func (c *Client) SessionExists(ctx context.Context, name, session string) (bool, error) {
	if !ValidSession(session) {
		return false, fmt.Errorf("invalid session %q", session)
	}
	var out struct {
		Exists bool `json:"exists"`
	}
	p, err := boxPath(name, "sessions", session)
	if err == nil {
		err = c.boxCall(ctx, http.MethodGet, p, &out)
	}
	return out.Exists, err
}

// SessionScrollback returns a bounded tmux capture, including terminal
// attributes, for a reconnecting terminal client.
func (c *Client) SessionScrollback(ctx context.Context, name, session string, lines int) ([]byte, error) {
	if !ValidSession(session) {
		return nil, fmt.Errorf("invalid session %q", session)
	}
	if lines < 1 || lines > 10000 {
		return nil, fmt.Errorf("lines: want 1 to 10000")
	}
	p, err := boxPath(name, "sessions", session, "scrollback")
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, http.MethodGet, PathBoxes+p+"?lines="+strconv.Itoa(lines), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNoBox
	}
	if resp.StatusCode >= 300 {
		return nil, readError(resp)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// ResizeSession changes the tmux window size for an existing session.
func (c *Client) ResizeSession(ctx context.Context, name, session string, size Resize) error {
	if !ValidSession(session) {
		return fmt.Errorf("invalid session %q", session)
	}
	if err := size.Validate(); err != nil {
		return err
	}
	p, err := boxPath(name, "sessions", session, "resize")
	if err != nil {
		return err
	}
	return c.boxSend(ctx, http.MethodPost, p, size, nil)
}

// KillSession ends a tmux session; one already gone is not an error.
func (c *Client) KillSession(ctx context.Context, name, session string) error {
	if !ValidSession(session) {
		return fmt.Errorf("invalid session %q", session)
	}
	p, err := boxPath(name, "sessions", session, "kill")
	if err != nil {
		return err
	}
	return c.boxCall(ctx, http.MethodPost, p, nil)
}

// TTY is an attached terminal in a box: Write sends input, Resize the
// client's size, Read the output until the process exits (io.EOF, with
// Exit set). Close detaches; the tmux session lives on.
type TTY struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
	buf  []byte
	// Exit is set once Read has returned io.EOF.
	Exit *Exit
}

// AttachTTY attaches a TTY to a tmux session in a running box.
func (c *Client) AttachTTY(ctx context.Context, name string, tr TTYRequest) (*TTY, error) {
	p, err := boxPath(name, "tty")
	if err != nil {
		return nil, err
	}
	if err := tr.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(tr)
	if err != nil {
		return nil, err
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	req, err := http.NewRequest(http.MethodPost, "http://"+c.host+PathBoxes+p, bytes.NewReader(body))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", TTYUpgrade)
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, ctxErr(ctx, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req) //nolint:bodyclose // an upgrade: the connection is the body, closed by Close
	if err != nil {
		_ = conn.Close()
		return nil, ctxErr(ctx, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = conn.Close() }()
		if resp.StatusCode == http.StatusNotFound {
			return nil, ErrNoBox
		}
		return nil, readError(resp)
	}
	return &TTY{conn: conn, br: br}, nil
}

// Write sends terminal input.
func (t *TTY) Write(p []byte) (int, error) {
	t.wmu.Lock()
	defer t.wmu.Unlock()
	if err := WriteChunked(t.conn, FrameStdin, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Resize sets the terminal's size.
func (t *TTY) Resize(cols, rows int) error {
	r := Resize{Cols: cols, Rows: rows}
	if err := r.Validate(); err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	t.wmu.Lock()
	defer t.wmu.Unlock()
	return WriteFrame(t.conn, FrameResize, b)
}

// Read reads terminal output. At the process's exit it returns io.EOF and
// sets Exit.
func (t *TTY) Read(p []byte) (int, error) {
	for len(t.buf) == 0 {
		if t.Exit != nil {
			return 0, io.EOF
		}
		typ, b, err := ReadFrame(t.br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("sandboxd: tty: stream ended without an exit status")
			}
			return 0, err
		}
		switch typ {
		case FrameStdout:
			t.buf = b
		case FrameExit:
			var ex Exit
			if err := json.Unmarshal(b, &ex); err != nil {
				return 0, fmt.Errorf("sandboxd: tty: exit frame: %w", err)
			}
			t.Exit = &ex
		default:
			return 0, fmt.Errorf("sandboxd: tty: unknown frame type %d", typ)
		}
	}
	n := copy(p, t.buf)
	t.buf = t.buf[n:]
	return n, nil
}

// Close detaches.
func (t *TTY) Close() error { return t.conn.Close() }

// AccountUsage reads the rate limits the box's status line last recorded
// for an account; no observation yet is a zero ObservedAt.
func (c *Client) AccountUsage(ctx context.Context, name, slot string) (AccountUsage, error) {
	return c.usage(ctx, http.MethodGet, name, slot, "usage")
}

// PollAccountUsage asks the account's usage of /api/oauth/usage from inside
// the box, with the account's own access token.
func (c *Client) PollAccountUsage(ctx context.Context, name, slot string) (AccountUsage, error) {
	return c.usage(ctx, http.MethodPost, name, slot, "usage", "poll")
}

func (c *Client) usage(ctx context.Context, method, name, slot string, rest ...string) (AccountUsage, error) {
	var u AccountUsage
	if !ValidSlot(slot) {
		return u, fmt.Errorf("invalid account slot %q", slot)
	}
	p, err := boxPath(name, append([]string{"accounts", slot}, rest...)...)
	if err == nil {
		err = c.boxCall(ctx, method, p, &u)
	}
	return u, err
}

// WriteGHToken puts a GitHub installation token at /run/orbit/gh-token in a
// running box, for its gh and git.
func (c *Client) WriteGHToken(ctx context.Context, name, token string) error {
	return c.ghToken(ctx, http.MethodPut, name, strings.NewReader(token))
}

// ClearGHToken removes it.
func (c *Client) ClearGHToken(ctx context.Context, name string) error {
	return c.ghToken(ctx, http.MethodDelete, name, nil)
}

func (c *Client) ghToken(ctx context.Context, method, name string, body io.Reader) error {
	p, err := boxPath(name, "gh-token")
	if err != nil {
		return err
	}
	ctype := ""
	if body != nil {
		ctype = "text/plain"
	}
	resp, err := c.do(ctx, method, PathBoxes+p, body, ctype)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoBox
	}
	if resp.StatusCode >= 300 {
		return readError(resp)
	}
	return nil
}

// AccountMCP lists an account's MCP servers and connectors, health-checked
// in the box.
func (c *Client) AccountMCP(ctx context.Context, name, slot string) ([]MCPServer, error) {
	var out struct {
		Servers []MCPServer `json:"servers"`
	}
	if !ValidSlot(slot) {
		return nil, fmt.Errorf("invalid account slot %q", slot)
	}
	p, err := boxPath(name, "accounts", slot, "mcp")
	if err == nil {
		err = c.boxCall(ctx, http.MethodGet, p, &out)
	}
	return out.Servers, err
}

// SyncAccountMCP mirrors the servers orbit manages into an account.
func (c *Client) SyncAccountMCP(ctx context.Context, name, slot string, servers map[string]json.RawMessage) error {
	if !ValidSlot(slot) {
		return fmt.Errorf("invalid account slot %q", slot)
	}
	if servers == nil {
		servers = map[string]json.RawMessage{}
	}
	if err := ValidateMCPServers(servers); err != nil {
		return err
	}
	p, err := boxPath(name, "accounts", slot, "mcp", "sync")
	if err == nil {
		err = c.boxSend(ctx, http.MethodPost, p, servers, nil)
	}
	return err
}

// SyncSkills puts a tar of <skill>/<file> into the box's shared profile.
func (c *Client) SyncSkills(ctx context.Context, name string, tarball io.Reader) error {
	p, err := boxPath(name, "skills")
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, PathBoxes+p, tarball, "application/x-tar")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNoBox
	}
	if resp.StatusCode >= 300 {
		return readError(resp)
	}
	return nil
}
