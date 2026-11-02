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
	"sync"
)

func (c *Client) boxCall(ctx context.Context, method, p string, out any) error {
	resp, err := c.do(ctx, method, PathBoxes+p, nil, "")
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
// returns it running.
func (c *Client) EnsureBox(ctx context.Context, name string) (BoxStatus, error) {
	var st BoxStatus
	p, err := boxPath(name)
	if err == nil {
		err = c.boxCall(ctx, http.MethodPost, p, &st)
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
