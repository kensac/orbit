// Package claudeproto speaks Claude Code's stream-json protocol: the CLI started with `--output-format stream-json --input-format stream-json` writes one JSON object per stdout line and reads one per stdin line, and both sides may send control requests the other answers.
//
// The wire format is the one the Agent SDK speaks to the same CLI (@anthropic-ai/claude-agent-sdk 0.3.316, matching the pinned CLI 2.1.316; core.mjs, class Query: readMessages, handleControlRequest, processControlRequest, request; the ProcessTransport argument builder; sdk.d.ts: PermissionResult, SDKUserMessage, SDKControlInitializeResponse). orbit does not run the SDK: it is a Node library, and orbit's runners are Go processes that must own the child, its environment and its process group. This package is the protocol alone; what a request is answered with is the caller's (Handler).
//
//	CLI -> host  {"type":"control_request","request_id":"<id>","request":{"subtype":"can_use_tool",...}}
//	host -> CLI  {"type":"control_response","response":{"subtype":"success","request_id":"<id>","response":{...}}}
//	             {"type":"control_response","response":{"subtype":"error","request_id":"<id>","error":"..."}}
//	CLI -> host  {"type":"control_cancel_request","request_id":"<id>"}  (the CLI withdrew a request)
//	host -> CLI  the same three shapes, for requests the host sends (initialize, interrupt, ...)
//	host -> CLI  {"type":"user","message":{"role":"user","content":"..."},"parent_tool_use_id":null}
//	CLI -> host  every other line: system, assistant, user, stream events, and one result per turn
package claudeproto

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kensac/orbit/internal/tokens"
)

// MaxLine bounds one line of the CLI's stream. A tool result echoing a large input file arrives as one line; 16 MB is twice orbit's default source cap.
const MaxLine = 16 << 20

// Line is one line the CLI wrote, with its type and subtype read.
type Line struct {
	// Kind is "type" or "type.subtype", or "raw" for a line that is not a JSON object with a type.
	Kind    string
	Type    string
	Subtype string
	Raw     []byte
}

// ParseLine reads a line's head. It never fails: a line that is not JSON is kept as Kind "raw" so a transcript loses nothing.
func ParseLine(raw []byte) Line {
	var h struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	l := Line{Kind: "raw", Raw: raw}
	if json.Unmarshal(raw, &h) != nil || h.Type == "" {
		return l
	}
	l.Type, l.Subtype, l.Kind = h.Type, h.Subtype, h.Type
	if h.Subtype != "" {
		l.Kind += "." + h.Subtype
	}
	return l
}

// IsResult reports whether the line ends a turn.
func (l Line) IsResult() bool { return l.Type == "result" }

// SessionID is the session the line names; every stream-json line from the CLI carries one, the system init line first.
func (l Line) SessionID() string {
	var h struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(l.Raw, &h)
	return h.SessionID
}

// UserMessage is one user turn as the CLI reads it on stdin (SDKUserMessage: a Messages API user message and parent_tool_use_id, null for a top-level turn). session_id is ignored by the CLI, which answers in the session it holds; orbit sends "default" as the SDK's examples do.
func UserMessage(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": text},
		"parent_tool_use_id": nil,
		"session_id":         "default",
	})
	return b
}

// Result is a turn's final line, the fields orbit reads. Usage and ModelUsageSince are the turn's own; ModelUsage is the process's running total by model.
type Result struct {
	IsError           bool            `json:"is_error"`
	Subtype           string          `json:"subtype"`
	Result            string          `json:"result"`
	Errors            []string        `json:"errors"`
	TerminalReason    string          `json:"terminal_reason"`
	StructuredOutput  json.RawMessage `json:"structured_output"`
	TotalCostUSD      *float64        `json:"total_cost_usd"`
	Usage             json.RawMessage `json:"usage"`
	ModelUsage        json.RawMessage `json:"modelUsage"`
	ModelUsageSince   json.RawMessage `json:"model_usage_since_last_result"`
	SessionID         string          `json:"session_id"`
	NumTurns          int             `json:"num_turns"`
	PermissionDenials json.RawMessage `json:"permission_denials"`
	// Raw is the whole line.
	Raw []byte `json:"-"`
}

// ParseResult reads a result line.
func ParseResult(raw []byte) (*Result, error) {
	r := &Result{Raw: raw}
	if err := json.Unmarshal(raw, r); err != nil {
		return nil, fmt.Errorf("parse the CLI's result: %w", err)
	}
	return r, nil
}

// Tokens is the turn's token counts, nil when the line has none.
func (r *Result) Tokens() *tokens.Usage {
	return tokens.FromClaude(r.Usage, r.ModelUsageSince, r.ModelUsage, r.NumTurns)
}

// Failure is what went wrong in an error result, empty for a success. is_error is the field to trust: a failed login reports subtype "success" with is_error set and the reason in result; the other error subtypes (error_max_turns, error_during_execution, ...) carry theirs in errors.
func (r *Result) Failure() string {
	if !r.IsError {
		return ""
	}
	if r.Subtype == "success" || len(r.Errors) == 0 {
		return strings.TrimSpace(r.Result)
	}
	var parts []string
	for _, e := range r.Errors {
		if e = strings.TrimSpace(e); e != "" {
			parts = append(parts, e)
		}
	}
	return strings.Join(parts, "; ")
}

// HasOutput reports whether the turn produced a --json-schema output.
func (r *Result) HasOutput() bool {
	return len(r.StructuredOutput) > 0 && string(r.StructuredOutput) != "null"
}

// controlEnvelope is a control_request or control_cancel_request line.
type controlEnvelope struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Request   json.RawMessage `json:"request"`
}

// responseEnvelope is a control_response line.
type responseEnvelope struct {
	Type     string          `json:"type"`
	Response controlResponse `json:"response"`
}

type controlResponse struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func successLine(id string, body any) []byte {
	raw, _ := json.Marshal(body)
	b, _ := json.Marshal(responseEnvelope{Type: "control_response",
		Response: controlResponse{Subtype: "success", RequestID: id, Response: raw}})
	return b
}

func errorLine(id, msg string) []byte {
	b, _ := json.Marshal(responseEnvelope{Type: "control_response",
		Response: controlResponse{Subtype: "error", RequestID: id, Error: msg}})
	return b
}
