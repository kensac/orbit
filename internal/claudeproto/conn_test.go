package claudeproto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// cli is the CLI's side of an in-memory Conn: what the host writes arrives on lines, and say writes a line to the host.
type cli struct {
	t     *testing.T
	out   *io.PipeWriter
	in    *bufio.Scanner
	conn  *Conn
	inRaw *io.PipeReader
}

func newCLI(t *testing.T, h Handler) *cli {
	t.Helper()
	hostR, cliW := io.Pipe()
	cliR, hostW := io.Pipe()
	c := &cli{t: t, out: cliW, inRaw: cliR, in: bufio.NewScanner(cliR)}
	c.conn = NewConn(hostR, hostW, h)
	t.Cleanup(func() { _ = cliW.Close(); _ = cliR.Close() })
	return c
}

func (c *cli) say(format string, args ...any) {
	c.t.Helper()
	if _, err := fmt.Fprintf(c.out, format+"\n", args...); err != nil {
		c.t.Fatalf("the CLI could not write: %v", err)
	}
}

// read is the next line the host wrote, decoded.
func (c *cli) read() map[string]any {
	c.t.Helper()
	got := make(chan map[string]any, 1)
	go func() {
		if !c.in.Scan() {
			got <- nil
			return
		}
		var m map[string]any
		_ = json.Unmarshal(c.in.Bytes(), &m)
		got <- m
	}()
	select {
	case m := <-got:
		if m == nil {
			c.t.Fatal("the host's input ended")
		}
		return m
	case <-time.After(5 * time.Second):
		c.t.Fatal("the host wrote nothing")
	}
	return nil
}

// silent asserts the host writes nothing for a while.
func (c *cli) silent(d time.Duration) {
	c.t.Helper()
	got := make(chan string, 1)
	go func() {
		if c.in.Scan() {
			got <- c.in.Text()
		}
	}()
	select {
	case l := <-got:
		c.t.Fatalf("the host wrote %s", l)
	case <-time.After(d):
		// The scanner goroutine is left reading; close the pipe so it ends.
		_ = c.inRaw.Close()
	}
}

func response(m map[string]any) map[string]any {
	r, _ := m["response"].(map[string]any)
	return r
}

func body(m map[string]any) map[string]any {
	b, _ := response(m)["response"].(map[string]any)
	return b
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestATurnSendsAUserMessageAndEndsAtItsResult(t *testing.T) {
	c := newCLI(t, Handler{})
	var kinds []string
	done := make(chan *Result, 1)
	go func() {
		r, err := c.conn.Turn(ctx(t), "hello", func(l Line) { kinds = append(kinds, l.Kind) })
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	msg := c.read()
	if msg["type"] != "user" || msg["parent_tool_use_id"] != nil {
		t.Fatalf("user message: %v", msg)
	}
	if m := msg["message"].(map[string]any); m["role"] != "user" || m["content"] != "hello" {
		t.Fatalf("message: %v", m)
	}
	c.say(`{"type":"system","subtype":"init","session_id":"s1"}`)
	c.say(`{"type":"keep_alive"}`)
	c.say(`not json`)
	c.say(`{"type":"assistant","session_id":"s1"}`)
	c.say(`{"type":"result","subtype":"success","is_error":false,"session_id":"s1","structured_output":{"a":1},"total_cost_usd":0.5,"num_turns":2}`)
	r := <-done
	if r == nil || r.SessionID != "s1" || !r.HasOutput() || *r.TotalCostUSD != 0.5 || r.NumTurns != 2 {
		t.Fatalf("result: %+v", r)
	}
	if got := strings.Join(kinds, ","); got != "system.init,raw,assistant,result.success" {
		t.Fatalf("lines handed on: %s (keep_alive is dropped, the rest kept in order)", got)
	}
}

func TestPermissionAnswers(t *testing.T) {
	var asked []PermissionRequest
	var mu sync.Mutex
	c := newCLI(t, Handler{CanUseTool: func(_ context.Context, r PermissionRequest) (Permission, error) {
		mu.Lock()
		asked = append(asked, r)
		mu.Unlock()
		switch r.Tool {
		case "Write":
			return Permission{Allow: true}, nil
		case "Edit":
			return Permission{Allow: true, UpdatedInput: json.RawMessage(`{"file_path":"b"}`)}, nil
		case "Bash":
			return Permission{Message: "not that command", Interrupt: true}, nil
		}
		return Permission{}, errors.New("orbit failed")
	}})
	c.say(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{"file_path":"a"},"tool_use_id":"t1","agent_id":"sub"}}`)
	m := c.read()
	if m["type"] != "control_response" || response(m)["subtype"] != "success" || response(m)["request_id"] != "r1" {
		t.Fatalf("response: %v", m)
	}
	if b := body(m); b["behavior"] != "allow" || b["toolUseID"] != "t1" || b["updatedInput"].(map[string]any)["file_path"] != "a" {
		t.Fatalf("an allow runs the input exactly as asked: %v", b)
	}
	c.say(`{"type":"control_request","request_id":"r2","request":{"subtype":"can_use_tool","tool_name":"Edit","input":{"file_path":"a"},"tool_use_id":"t2"}}`)
	if b := body(c.read()); b["updatedInput"].(map[string]any)["file_path"] != "b" {
		t.Fatalf("an updated input is sent: %v", b)
	}
	c.say(`{"type":"control_request","request_id":"r3","request":{"subtype":"can_use_tool","tool_name":"Bash","input":"not an object","tool_use_id":"t3"}}`)
	if b := body(c.read()); b["behavior"] != "deny" || b["message"] != "not that command" || b["interrupt"] != true {
		t.Fatalf("deny: %v", b)
	}
	c.say(`{"type":"control_request","request_id":"r4","request":{"subtype":"can_use_tool","tool_name":"Read","input":{}}}`)
	if r := response(c.read()); r["subtype"] != "error" || r["error"] != "orbit failed" || r["request_id"] != "r4" {
		t.Fatalf("a handler error is an error response: %v", r)
	}
	mu.Lock()
	defer mu.Unlock()
	if asked[0].AgentID != "sub" || asked[0].ToolUseID != "t1" || asked[0].RequestID != "r1" {
		t.Fatalf("request fields: %+v", asked[0])
	}
	if string(asked[2].Input) != "{}" {
		t.Fatalf("an input that is not an object is read as {}: %s", asked[2].Input)
	}
}

func TestAnUnhandledRequestIsRefusedAtOnce(t *testing.T) {
	c := newCLI(t, Handler{Name: "orbit's build"})
	for i, sub := range []string{"can_use_tool", "hook_callback", "mcp_message", "elicitation"} {
		c.say(`{"type":"control_request","request_id":"r%d","request":{"subtype":%q}}`, i, sub)
		r := response(c.read())
		if r["subtype"] != "error" || r["error"] != "orbit's build does not handle "+sub+" requests" || r["request_id"] != fmt.Sprintf("r%d", i) {
			t.Fatalf("%s: %v", sub, r)
		}
	}
}

func TestHookAndMCPAnswers(t *testing.T) {
	c := newCLI(t, Handler{
		Hook: func(_ context.Context, r HookCallback) (json.RawMessage, error) {
			return json.RawMessage(fmt.Sprintf(`{"decision":"block","reason":%q}`, r.CallbackID+" "+r.ToolUseID)), nil
		},
		MCP: func(_ context.Context, r MCPMessage) (json.RawMessage, error) {
			var m struct {
				ID int `json:"id"`
			}
			_ = json.Unmarshal(r.Message, &m)
			return json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{"server":%q}}`, m.ID, r.ServerName)), nil
		},
	})
	c.say(`{"type":"control_request","request_id":"h","request":{"subtype":"hook_callback","callback_id":"hook_0","input":{"hook_event_name":"PreToolUse"},"tool_use_id":"t"}}`)
	if b := body(c.read()); b["decision"] != "block" || b["reason"] != "hook_0 t" {
		t.Fatalf("hook output: %v", b)
	}
	c.say(`{"type":"control_request","request_id":"m","request":{"subtype":"mcp_message","server_name":"orbit","message":{"jsonrpc":"2.0","id":7,"method":"tools/list"}}}`)
	b := body(c.read())
	reply, _ := b["mcp_response"].(map[string]any)
	if reply["id"] != float64(7) || reply["result"].(map[string]any)["server"] != "orbit" {
		t.Fatalf("an MCP reply is wrapped in mcp_response: %v", b)
	}
}

func TestADuplicateDeliveryIsAnsweredOnce(t *testing.T) {
	release := make(chan struct{})
	calls := 0
	var mu sync.Mutex
	c := newCLI(t, Handler{CanUseTool: func(context.Context, PermissionRequest) (Permission, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return Permission{Allow: true}, nil
	}})
	line := `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{}}}`
	c.say(line)
	c.say(line)
	time.Sleep(50 * time.Millisecond)
	close(release)
	c.read()
	c.silent(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("decided %d times", calls)
	}
}

func TestAWithdrawnRequestIsNotAnswered(t *testing.T) {
	withdrawn := make(chan struct{})
	c := newCLI(t, Handler{CanUseTool: func(ctx context.Context, _ PermissionRequest) (Permission, error) {
		<-ctx.Done()
		close(withdrawn)
		return Permission{Allow: true}, nil
	}})
	c.say(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{}}}`)
	c.say(`{"type":"control_cancel_request","request_id":"r1"}`)
	select {
	case <-withdrawn:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler's context did not end")
	}
	c.silent(200 * time.Millisecond)
}

func TestASlowDecisionDoesNotStopTheStream(t *testing.T) {
	release := make(chan struct{})
	c := newCLI(t, Handler{CanUseTool: func(context.Context, PermissionRequest) (Permission, error) {
		<-release
		return Permission{Allow: true}, nil
	}})
	c.say(`{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{}}}`)
	c.say(`{"type":"assistant"}`)
	for _, want := range []string{"control_request", "assistant"} {
		l, err := c.conn.Next(ctx(t))
		if err != nil || l.Kind != want {
			t.Fatalf("next: %v %v, want %s", l.Kind, err, want)
		}
	}
	close(release)
	if body(c.read())["behavior"] != "allow" {
		t.Fatal("no answer")
	}
}

func TestRequestsTheHostSends(t *testing.T) {
	c := newCLI(t, Handler{})
	type answer struct {
		body json.RawMessage
		err  error
	}
	got := make(chan answer, 1)
	go func() {
		b, err := c.conn.Initialize(ctx(t), Init{
			Hooks:      map[string][]HookMatcher{"PreToolUse": {{Matcher: "Read", CallbackIDs: []string{"hook_0"}, Timeout: 10}}},
			MCPServers: []string{"orbit"},
		})
		got <- answer{b, err}
	}()
	m := c.read()
	req := m["request"].(map[string]any)
	id, _ := m["request_id"].(string)
	if m["type"] != "control_request" || req["subtype"] != "initialize" || id == "" {
		t.Fatalf("initialize: %v", m)
	}
	hooks := req["hooks"].(map[string]any)["PreToolUse"].([]any)[0].(map[string]any)
	if hooks["matcher"] != "Read" || hooks["hookCallbackIds"].([]any)[0] != "hook_0" || hooks["timeout"] != float64(10) {
		t.Fatalf("hooks: %v", hooks)
	}
	if req["sdkMcpServers"].([]any)[0] != "orbit" {
		t.Fatalf("servers: %v", req)
	}
	c.say(`{"type":"control_response","response":{"subtype":"success","request_id":"other","response":{}}}`)
	c.say(`{"type":"control_response","response":{"subtype":"success","request_id":%q,"response":{"commands":[]}}}`, id)
	if a := <-got; a.err != nil || string(a.body) != `{"commands":[]}` {
		t.Fatalf("initialize answered %s %v", a.body, a.err)
	}

	go func() { got <- answer{nil, c.conn.SetPermissionMode(ctx(t), "plan")} }()
	m = c.read()
	if req := m["request"].(map[string]any); req["subtype"] != "set_permission_mode" || req["mode"] != "plan" {
		t.Fatalf("set_permission_mode: %v", m)
	}
	c.say(`{"type":"control_response","response":{"subtype":"error","request_id":%q,"error":"no such mode"}}`, m["request_id"])
	var ce *ControlError
	if a := <-got; !errors.As(a.err, &ce) || ce.Subtype != "set_permission_mode" || ce.Message != "no such mode" {
		t.Fatalf("a refusal is a ControlError: %v", a.err)
	}

	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go func() { got <- answer{nil, c.conn.Interrupt(short)} }()
	m = c.read()
	if m["request"].(map[string]any)["subtype"] != "interrupt" {
		t.Fatalf("interrupt: %v", m)
	}
	withdraw := c.read()
	if withdraw["type"] != "control_cancel_request" || withdraw["request_id"] != m["request_id"] {
		t.Fatalf("an unanswered request is withdrawn: %v", withdraw)
	}
	if a := <-got; !errors.Is(a.err, context.DeadlineExceeded) {
		t.Fatalf("timeout: %v", a.err)
	}

	go func() { got <- answer{nil, c.conn.SetModel(ctx(t), "")} }()
	m = c.read()
	if req := m["request"].(map[string]any); req["subtype"] != "set_model" || req["model"] != nil {
		t.Fatalf("set_model to the default sends null: %v", m)
	}
	_ = c.out.Close()
	if a := <-got; !errors.Is(a.err, ErrEnded) {
		t.Fatalf("a request the CLI never answered: %v", a.err)
	}
	if _, err := c.conn.Request(ctx(t), "interrupt", nil); !errors.Is(err, ErrEnded) {
		t.Fatalf("a request after the end: %v", err)
	}
}

func TestNextAfterTheEnd(t *testing.T) {
	c := newCLI(t, Handler{})
	c.say(`{"type":"assistant"}`)
	_ = c.out.Close()
	<-c.conn.Done()
	if l, err := c.conn.Next(ctx(t)); err != nil || l.Kind != "assistant" {
		t.Fatalf("a queued line comes before the end: %v %v", l.Kind, err)
	}
	if _, err := c.conn.Next(ctx(t)); !errors.Is(err, ErrEnded) {
		t.Fatalf("then the end: %v", err)
	}
	if _, err := c.conn.Turn(ctx(t), "x", nil); err == nil {
		t.Fatal("a turn on an ended stream")
	}
}

func TestRecordedPermissions(t *testing.T) {
	allow := func(context.Context, PermissionRequest) Permission { return Permission{Allow: true} }
	deny := func(context.Context, PermissionRequest) Permission { return Permission{Message: "no"} }
	failing := func(PermissionRequest, Permission) error { return errors.New("db down") }
	req := PermissionRequest{Tool: "Write"}

	if p, _ := Recorded(allow, nil)(context.Background(), req); p.Allow {
		t.Fatal("nothing is allowed without a record")
	}
	if p, _ := Recorded(allow, failing)(context.Background(), req); p.Allow || !strings.Contains(p.Message, "could not record") {
		t.Fatalf("an unrecorded allow is a deny: %+v", p)
	}
	if p, _ := Recorded(deny, failing)(context.Background(), req); p.Allow || p.Message != "no" {
		t.Fatalf("an unrecorded deny keeps its message: %+v", p)
	}

	// The decision is stored before the CLI hears it.
	in := &logWriter{wrote: make(chan struct{}, 1)}
	hostR, cliW := io.Pipe()
	t.Cleanup(func() { _ = cliW.Close() })
	var linesAtRecord int
	record := func(PermissionRequest, Permission) error {
		linesAtRecord = in.count()
		return nil
	}
	NewConn(hostR, in, Handler{CanUseTool: Recorded(allow, record)})
	fmt.Fprintln(cliW, `{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Write","input":{}}}`)
	select {
	case <-in.wrote:
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}
	if linesAtRecord != 0 || !strings.Contains(in.String(), `"behavior":"allow"`) {
		t.Fatalf("answer %s, lines written before the record: %d", in.String(), linesAtRecord)
	}
}

// logWriter is a CLI stdin that keeps what the host wrote.
type logWriter struct {
	mu    sync.Mutex
	b     strings.Builder
	n     int
	wrote chan struct{}
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.b.Write(p)
	w.n++
	w.mu.Unlock()
	select {
	case w.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (w *logWriter) Close() error { return nil }

func (w *logWriter) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
}

func (w *logWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestLastLineCountsKeepAlives(t *testing.T) {
	c := newCLI(t, Handler{})
	if !c.conn.LastLine().IsZero() {
		t.Fatal("a line before the CLI wrote one")
	}
	before := time.Now()
	c.say(`{"type":"keep_alive"}`)
	deadline := time.Now().Add(5 * time.Second)
	for c.conn.LastLine().Before(before) {
		if time.Now().After(deadline) {
			t.Fatal("a keep_alive was not counted")
		}
		time.Sleep(time.Millisecond)
	}
}
