package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/kensac/orbit/internal/claudeproto"
)

// The CLI's stdio permission protocol (internal/claudeproto): with --permission-prompt-tool stdio the CLI sends a can_use_tool control request for every tool call its permission rules leave as "ask" and waits for orbit's answer; any other control request is refused. Seen against the real CLI 2.1.316 (TestRealCLIPermissionProtocol): an allowed Write wrote, a denied one did not and the model was told orbit's message, and a deny rule refused a command without asking.

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

// handler answers the CLI's requests: can_use_tool through the policy,
// each decision recorded before it is sent (claudeproto.Recorded); every
// other subtype is refused.
func (cv *conversation) handler() claudeproto.Handler {
	decide := func(_ context.Context, r claudeproto.PermissionRequest) claudeproto.Permission {
		d := cv.perms.Policy.Decide(r.Tool, r.Input)
		return claudeproto.Permission{Allow: d.Allow, Message: d.Message}
	}
	var record func(claudeproto.PermissionRequest, claudeproto.Permission) error
	if cv.perms.Record != nil {
		record = func(r claudeproto.PermissionRequest, p claudeproto.Permission) error {
			return cv.perms.Record(PermissionRequest{RequestID: r.RequestID, Tool: r.Tool, Input: r.Input}, Decision{Allow: p.Allow, Message: p.Message})
		}
	}
	recorded := claudeproto.Recorded(decide, record)
	return claudeproto.Handler{
		Name: "orbit's build",
		CanUseTool: func(ctx context.Context, r claudeproto.PermissionRequest) (claudeproto.Permission, error) {
			p, err := recorded(ctx, r)
			ev, _ := json.Marshal(map[string]any{"request_id": r.RequestID, "tool": r.Tool, "allowed": p.Allow, "message": p.Message})
			cv.emit("orbit.permission", ev)
			return p, err
		},
	}
}

// run drives proc to its result. It returns the result line, or an error
// when the process ended without one.
func (cv *conversation) run(ctx context.Context, proc Process, prompt string) (*claudeproto.Result, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	stderr := &claudeproto.Tail{}
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

	conn := claudeproto.NewConn(outR, inW, cv.handler())
	res, convErr := conn.Turn(ctx, prompt, func(l claudeproto.Line) { cv.emit(l.Kind, l.Raw) })
	// The result ends the conversation: closing stdin makes the CLI exit.
	// On any error, cancelling ends the attach too.
	_ = conn.CloseInput()
	if convErr != nil {
		cancel()
	}
	<-conn.Done()
	ex := <-done
	if res != nil {
		return res, nil
	}
	if convErr == nil || errors.Is(convErr, claudeproto.ErrEnded) {
		convErr = errors.New("the CLI exited without a result")
		if err := conn.Err(); err != nil {
			convErr = err
		}
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
