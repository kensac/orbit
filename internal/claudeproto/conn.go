package claudeproto

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// RequestTimeout bounds a control request the host sends when the caller's context has no deadline of its own. The CLI answers interrupt, set_permission_mode and set_model at once; a request still unanswered after this long has an unknown outcome (the CLI may yet apply it).
const RequestTimeout = 30 * time.Second

// Conn is one stream-json conversation with a CLI: lines in from its stdout, lines out to its stdin. It reads for its whole life, answering the CLI's control requests through Handler and matching the answers to requests the host sent, so neither side ever waits on a line nobody reads. Every other line is queued for Next, unbounded: between turns the CLI writes nothing, so the queue is at most one turn.
type Conn struct {
	h Handler

	wmu sync.Mutex
	w   io.WriteCloser

	mu       sync.Mutex
	queue    []Line
	ready    chan struct{} // signalled when queue grows or the stream ends
	pending  map[string]chan controlResponse
	inflight map[string]context.CancelFunc
	ended    bool
	endErr   error

	done chan struct{}
}

// NewConn starts reading r, the CLI's stdout; w is its stdin.
func NewConn(r io.Reader, w io.WriteCloser, h Handler) *Conn {
	c := &Conn{
		h: h, w: w, ready: make(chan struct{}, 1),
		pending: map[string]chan controlResponse{}, inflight: map[string]context.CancelFunc{},
		done: make(chan struct{}),
	}
	go c.read(r)
	return c
}

// Done is closed once the stream has ended.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the stream ended: nil for a clean end of output, else the read error. Meaningful once Done is closed.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endErr
}

func (c *Conn) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), MaxLine)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		c.dispatch(ParseLine(append([]byte(nil), raw...)))
	}
	err := sc.Err()
	if err != nil {
		err = fmt.Errorf("read the CLI's stream: %w", err)
		// Drain so the CLI is never blocked writing to a full pipe.
		_, _ = io.Copy(io.Discard, r)
	}
	c.end(err)
}

// dispatch acts on one line: control traffic is handled here, keep_alive dropped, and every line but keep_alive is queued for Next, control lines included, so the transcript holds the whole exchange.
func (c *Conn) dispatch(l Line) {
	switch l.Type {
	case "keep_alive":
		return
	case "control_request":
		var env controlEnvelope
		if json.Unmarshal(l.Raw, &env) == nil && env.RequestID != "" {
			c.handle(env.RequestID, env.Request)
		}
	case "control_cancel_request":
		var env controlEnvelope
		if json.Unmarshal(l.Raw, &env) == nil {
			c.mu.Lock()
			cancel := c.inflight[env.RequestID]
			delete(c.inflight, env.RequestID)
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
	case "control_response":
		var env responseEnvelope
		if json.Unmarshal(l.Raw, &env) == nil {
			c.mu.Lock()
			ch := c.pending[env.Response.RequestID]
			delete(c.pending, env.Response.RequestID)
			c.mu.Unlock()
			if ch != nil {
				ch <- env.Response
			}
		}
	}
	c.mu.Lock()
	c.queue = append(c.queue, l)
	c.mu.Unlock()
	c.signal()
}

// handle answers one of the CLI's requests on its own goroutine, so a slow decision never stops the stream. A request id already in flight is a duplicate delivery and is ignored, so no request is answered twice.
func (c *Conn) handle(id string, raw json.RawMessage) {
	c.mu.Lock()
	if _, dup := c.inflight[id]; dup || c.ended {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.inflight[id] = cancel
	c.mu.Unlock()
	go func() {
		defer cancel()
		line := c.h.answer(ctx, id, raw)
		c.mu.Lock()
		live := c.inflight[id] != nil
		delete(c.inflight, id)
		c.mu.Unlock()
		if line != nil && live {
			_ = c.WriteLine(line)
		}
	}()
}

func (c *Conn) end(err error) {
	c.mu.Lock()
	c.ended, c.endErr = true, err
	pending, inflight := c.pending, c.inflight
	c.pending, c.inflight = map[string]chan controlResponse{}, map[string]context.CancelFunc{}
	c.mu.Unlock()
	for _, cancel := range inflight {
		cancel()
	}
	for _, ch := range pending {
		close(ch)
	}
	close(c.done)
	c.signal()
}

func (c *Conn) signal() {
	select {
	case c.ready <- struct{}{}:
	default:
	}
}

// Next returns the next line the CLI wrote, in order. After the last line it returns ErrEnded (wrapping the read error, if any).
func (c *Conn) Next(ctx context.Context) (Line, error) {
	for {
		c.mu.Lock()
		if len(c.queue) > 0 {
			l := c.queue[0]
			c.queue[0] = Line{}
			c.queue = c.queue[1:]
			c.mu.Unlock()
			return l, nil
		}
		ended, err := c.ended, c.endErr
		c.mu.Unlock()
		if ended {
			if err != nil {
				return Line{}, fmt.Errorf("%w: %w", ErrEnded, err)
			}
			return Line{}, ErrEnded
		}
		select {
		case <-ctx.Done():
			return Line{}, ctx.Err()
		case <-c.ready:
		}
	}
}

// WriteLine writes one line to the CLI's stdin.
func (c *Conn) WriteLine(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.w == nil {
		return errors.New("the CLI's input is closed")
	}
	_, err := c.w.Write(append(b[:len(b):len(b)], '\n'))
	return err
}

// Send writes one user turn. It fails with ErrEnded once the stream has ended: the CLI is gone, and a write to its stdin could block or vanish.
func (c *Conn) Send(text string) error {
	select {
	case <-c.done:
		return ErrEnded
	default:
	}
	if err := c.WriteLine(UserMessage(text)); err != nil {
		return fmt.Errorf("write the prompt to the CLI: %w", err)
	}
	return nil
}

// CloseInput closes the CLI's stdin: it finishes the turn it is in, if any, and exits.
func (c *Conn) CloseInput() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.w == nil {
		return nil
	}
	err := c.w.Close()
	c.w = nil
	return err
}

// Turn sends prompt and returns the result line that ends its answer, handing every line to emit on the way, the result included. It returns ErrEnded when the stream ends first.
func (c *Conn) Turn(ctx context.Context, prompt string, emit func(Line)) (*Result, error) {
	if err := c.Send(prompt); err != nil {
		return nil, err
	}
	return c.Await(ctx, emit)
}

// Await reads lines to emit until a result, for a turn already sent (a one-shot CLI whose prompt went in another way).
func (c *Conn) Await(ctx context.Context, emit func(Line)) (*Result, error) {
	for {
		l, err := c.Next(ctx)
		if err != nil {
			return nil, err
		}
		if emit != nil {
			emit(l)
		}
		if l.IsResult() {
			return ParseResult(l.Raw)
		}
	}
}

// Request sends a control request and returns the CLI's response body. A request still unanswered when ctx ends is withdrawn with a control_cancel_request, as the SDK withdraws an aborted one. Without a deadline on ctx, RequestTimeout applies.
func (c *Conn) Request(ctx context.Context, subtype string, fields map[string]any) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, RequestTimeout)
		defer cancel()
	}
	body := map[string]any{}
	for k, v := range fields {
		body[k] = v
	}
	body["subtype"] = subtype
	id := newRequestID()
	ch := make(chan controlResponse, 1)
	c.mu.Lock()
	if c.ended {
		c.mu.Unlock()
		return nil, ErrEnded
	}
	c.pending[id] = ch
	c.mu.Unlock()
	line, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": id, "request": body})
	if err := c.WriteLine(line); err != nil {
		c.forget(id)
		return nil, fmt.Errorf("send %s to the CLI: %w", subtype, err)
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, ErrEnded
		}
		if resp.Subtype != "success" {
			return nil, &ControlError{Subtype: subtype, Message: resp.Error}
		}
		return resp.Response, nil
	case <-ctx.Done():
		c.forget(id)
		cancel, _ := json.Marshal(map[string]any{"type": "control_cancel_request", "request_id": id})
		_ = c.WriteLine(cancel)
		return nil, fmt.Errorf("claude %s: %w", subtype, ctx.Err())
	}
}

func (c *Conn) forget(id string) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Initialize sends the initialize request, which registers host-side hooks and SDK MCP servers (keyed by event, each matcher naming callback ids the CLI later sends back in hook_callback), and returns the CLI's description of itself (commands, models, account). A CLI started without --await-initialize answers user messages without one; orbit needs it only to register callbacks.
func (c *Conn) Initialize(ctx context.Context, init Init) (json.RawMessage, error) {
	fields := map[string]any{}
	if len(init.Hooks) > 0 {
		fields["hooks"] = init.Hooks
	}
	if len(init.MCPServers) > 0 {
		fields["sdkMcpServers"] = init.MCPServers
	}
	return c.Request(ctx, "initialize", fields)
}

// Init is what an initialize request registers.
type Init struct {
	// Hooks maps a hook event (PreToolUse, ...) to its matchers.
	Hooks map[string][]HookMatcher
	// MCPServers names SDK MCP servers whose messages arrive as mcp_message.
	MCPServers []string
}

// HookMatcher is one matcher of an initialize request's hooks.
type HookMatcher struct {
	Matcher     string   `json:"matcher,omitempty"`
	CallbackIDs []string `json:"hookCallbackIds"`
	// Timeout is in seconds, the CLI's unit.
	Timeout int `json:"timeout,omitempty"`
}

// Interrupt stops the turn in progress; the CLI ends it with a result.
func (c *Conn) Interrupt(ctx context.Context) error {
	_, err := c.Request(ctx, "interrupt", nil)
	return err
}

// SetPermissionMode changes the session's permission mode (default, acceptEdits, bypassPermissions, plan, dontAsk, auto).
func (c *Conn) SetPermissionMode(ctx context.Context, mode string) error {
	_, err := c.Request(ctx, "set_permission_mode", map[string]any{"mode": mode})
	return err
}

// SetModel changes the model for later turns; empty restores the default.
func (c *Conn) SetModel(ctx context.Context, model string) error {
	var m any
	if model != "" {
		m = model
	}
	_, err := c.Request(ctx, "set_model", map[string]any{"model": m})
	return err
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "orbit_" + hex.EncodeToString(b)
}
