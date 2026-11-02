package build

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// The CLI's stdio permission protocol, as the Agent SDK speaks it
// (@anthropic-ai/claude-agent-sdk 0.3.316, core.mjs: the transport passes
// --permission-prompt-tool=stdio when a host supplies canUseTool, and
// Query.handleControlRequest answers). With that flag the CLI writes, for
// every tool call its permission rules leave as "ask",
//
//	{"type":"control_request","request_id":"<id>",
//	 "request":{"subtype":"can_use_tool","tool_name":"Write","input":{...},"tool_use_id":"..."}}
//
// on stdout and waits for, on stdin,
//
//	{"type":"control_response","response":{"subtype":"success","request_id":"<id>",
//	 "response":{"behavior":"allow","updatedInput":{...}}}}
//
// or {"behavior":"deny","message":"..."}. Any other control request gets
// {"subtype":"error","request_id":"<id>","error":"..."}, as the SDK answers
// a subtype it has no handler for. Seen against the real CLI 2.1.316
// (TestRealCLIPermissionProtocol): an allowed Write wrote, a denied one did
// not and the model was told orbit's message, and a deny rule refused a
// command without asking.

// maxLine bounds one line of the CLI's stream: a tool result echoing a large
// file is one line.
const maxLine = 16 << 20

// PermissionRequest is one can_use_tool request.
type PermissionRequest struct {
	RequestID string
	Tool      string
	Input     json.RawMessage
}

// Permissions answers a CLI's permission requests.
type Permissions struct {
	Policy Policy
	// Record stores the decision before it is sent. When it fails the
	// request is denied: an allow that was not recorded is never given.
	Record func(PermissionRequest, Decision) error
}

// decide is the whole answer to one request, recorded first.
func (p Permissions) decide(req PermissionRequest) Decision {
	d := p.Policy.Decide(req.Tool, req.Input)
	if p.Record == nil {
		return deny("orbit cannot record decisions, so it allows nothing")
	}
	if err := p.Record(req, d); err != nil {
		if d.Allow {
			return deny("orbit could not record this decision, so it is denied; try again")
		}
	}
	return d
}

// cliResult is the CLI's final result line, the parts orbit uses.
type cliResult struct {
	IsError           bool            `json:"is_error"`
	Subtype           string          `json:"subtype"`
	Result            string          `json:"result"`
	TerminalReason    string          `json:"terminal_reason"`
	StructuredOutput  json.RawMessage `json:"structured_output"`
	TotalCostUSD      *float64        `json:"total_cost_usd"`
	Usage             json.RawMessage `json:"usage"`
	NumTurns          int             `json:"num_turns"`
	PermissionDenials json.RawMessage `json:"permission_denials"`
	raw               []byte
}

// Process runs the CLI with these streams and returns its exit code: a
// sandbox attach in production, a local process in the real-CLI check.
type Process func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error)

// conversation drives one CLI process: the prompt in, every line of its
// stream to emit, every control request answered, stdin closed at the
// result so the CLI exits.
type conversation struct {
	perms Permissions
	emit  func(kind string, payload []byte)
}

// run drives proc to its result. It returns the result line, or an error
// when the process ended without one.
func (cv *conversation) run(ctx context.Context, proc Process, prompt string) (*cliResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	stderr := &tailBuffer{}
	type exit struct {
		code int
		err  error
	}
	done := make(chan exit, 1)
	go func() {
		code, err := proc(ctx, inR, outW, stderr)
		_ = outW.Close()
		_ = inR.Close()
		done <- exit{code, err}
	}()

	w := &lineWriter{w: inW}
	msg, _ := json.Marshal(map[string]any{
		"type": "user", "message": map[string]any{"role": "user", "content": prompt},
		"parent_tool_use_id": nil, "session_id": "default",
	})
	var res *cliResult
	convErr := w.line(msg)
	if convErr == nil {
		res, convErr = cv.read(outR, w)
	}
	// The result ends the conversation: closing stdin makes the CLI exit.
	// On any error, cancelling ends the attach too.
	_ = inW.Close()
	if convErr != nil {
		cancel()
	}
	_, _ = io.Copy(io.Discard, outR)
	ex := <-done
	if res != nil {
		return res, nil
	}
	if convErr == nil {
		convErr = errors.New("the CLI exited without a result")
	}
	if ex.err != nil && !errors.Is(ex.err, context.Canceled) {
		convErr = fmt.Errorf("%w (%w)", convErr, ex.err)
	} else if ex.code != 0 {
		convErr = fmt.Errorf("%w (exit %d)", convErr, ex.code)
	}
	if tail := strings.TrimSpace(stderr.String()); tail != "" {
		convErr = fmt.Errorf("%w: %s", convErr, lastLine(tail))
	}
	return nil, convErr
}

// read consumes the stream until the result line.
func (cv *conversation) read(out io.Reader, w *lineWriter) (*cliResult, error) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		line = append([]byte(nil), line...)
		var head struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype  string          `json:"subtype"`
				ToolName string          `json:"tool_name"`
				Input    json.RawMessage `json:"input"`
			} `json:"request"`
		}
		if json.Unmarshal(line, &head) != nil {
			cv.emit("raw", line)
			continue
		}
		kind := head.Type
		if head.Subtype != "" {
			kind += "." + head.Subtype
		}
		cv.emit(kind, line)
		switch head.Type {
		case "control_request":
			if err := w.line(cv.answer(head.RequestID, head.Request.Subtype, head.Request.ToolName, head.Request.Input)); err != nil {
				return nil, fmt.Errorf("answer the CLI's %s request: %w", head.Request.Subtype, err)
			}
		case "result":
			res := &cliResult{raw: line}
			if err := json.Unmarshal(line, res); err != nil {
				return nil, fmt.Errorf("parse the CLI's result: %w", err)
			}
			return res, nil
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read the CLI's stream: %w", err)
	}
	return nil, nil
}

// answer builds the control_response for one control request.
func (cv *conversation) answer(id, subtype, tool string, input json.RawMessage) []byte {
	var resp map[string]any
	if subtype == "can_use_tool" && id != "" {
		if len(input) == 0 || input[0] != '{' {
			input = json.RawMessage(`{}`)
		}
		d := cv.perms.decide(PermissionRequest{RequestID: id, Tool: tool, Input: input})
		ev, _ := json.Marshal(map[string]any{"request_id": id, "tool": tool, "allowed": d.Allow, "message": d.Message})
		cv.emit("orbit.permission", ev)
		r := map[string]any{"behavior": "deny", "message": d.Message}
		if d.Allow {
			// The input exactly as asked: orbit decides, never rewrites.
			r = map[string]any{"behavior": "allow", "updatedInput": input}
		}
		resp = map[string]any{"subtype": "success", "request_id": id, "response": r}
	} else {
		resp = map[string]any{"subtype": "error", "request_id": id,
			"error": "orbit does not handle " + subtype + " requests in a build"}
	}
	b, _ := json.Marshal(map[string]any{"type": "control_response", "response": resp})
	return b
}

// lineWriter writes whole lines to the CLI's stdin.
type lineWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lineWriter) line(b []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := l.w.Write(append(b, '\n'))
	return err
}

// tailBuffer keeps the last 8 KB written to it, for an error message.
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > 8<<10 {
		t.b = append(t.b[:0], t.b[len(t.b)-8<<10:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 500 {
		s = s[:500]
	}
	return s
}
