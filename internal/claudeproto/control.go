package claudeproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Request subtypes the CLI sends the host. With --permission-prompt-tool stdio the CLI asks can_use_tool for every call its rules leave as "ask"; hook_callback and mcp_message arrive only for hooks and SDK MCP servers an initialize request registered.
const (
	SubtypeCanUseTool   = "can_use_tool"
	SubtypeHookCallback = "hook_callback"
	SubtypeMCPMessage   = "mcp_message"
)

// PermissionRequest is one can_use_tool request (core.mjs, processControlRequest: the fields the SDK hands canUseTool).
type PermissionRequest struct {
	RequestID string `json:"-"`
	Tool      string `json:"tool_name"`
	// Input is the tool call's input, always a JSON object.
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	// AgentID names the subagent that asked; empty for the session's own.
	AgentID        string          `json:"agent_id"`
	BlockedPath    string          `json:"blocked_path"`
	DecisionReason string          `json:"decision_reason"`
	Suggestions    json.RawMessage `json:"permission_suggestions"`
	// DecisionReasonType is why the ask escalated, as a word: rule, mode, workingDir, safetyCheck, classifier, ... (sdk.d.ts SDKControlPermissionRequest).
	DecisionReasonType string `json:"decision_reason_type"`
	// MatchedRule is the permissions.ask rule that forced the prompt, {source, tool_name, rule_content}, when one did.
	MatchedRule json.RawMessage `json:"matched_ask_rule"`
	Title       string          `json:"title"`
	DisplayName string          `json:"display_name"`
	Description string          `json:"description"`
	// SuppressAlwaysAllow is an ask whose "don't ask again" rule would grant more than this call.
	SuppressAlwaysAllow bool `json:"suppress_always_allow_rule"`
	// RequiresUserInteraction is an ask a stored grant or an automatic approval must not answer (ExitPlanMode's, a classifier's block handed to a person).
	RequiresUserInteraction bool `json:"requires_user_interaction"`
}

// Permission is the answer to a PermissionRequest (sdk.d.ts PermissionResult).
type Permission struct {
	Allow bool
	// Message tells the model why a call was denied.
	Message string
	// Interrupt, with a deny, also ends the turn.
	Interrupt bool
	// UpdatedInput is the input the tool runs with; nil runs it exactly as asked. The CLI requires one on an allow, so the request's own is sent.
	UpdatedInput json.RawMessage
}

func (p Permission) body(req PermissionRequest) map[string]any {
	if p.Allow {
		in := p.UpdatedInput
		if len(in) == 0 {
			in = req.Input
		}
		return map[string]any{"behavior": "allow", "updatedInput": in, "toolUseID": req.ToolUseID}
	}
	m := map[string]any{"behavior": "deny", "message": p.Message, "toolUseID": req.ToolUseID}
	if p.Interrupt {
		m["interrupt"] = true
	}
	return m
}

// HookCallback is one hook_callback request: a hook an initialize request registered by callback id has fired.
type HookCallback struct {
	RequestID  string          `json:"-"`
	CallbackID string          `json:"callback_id"`
	Input      json.RawMessage `json:"input"`
	ToolUseID  string          `json:"tool_use_id"`
}

// MCPMessage is one mcp_message request: a JSON-RPC message for an SDK MCP server (one declared as {"type":"sdk","name":...}). The answer is the server's JSON-RPC reply, sent back as {"mcp_response": reply}.
type MCPMessage struct {
	RequestID  string          `json:"-"`
	ServerName string          `json:"server_name"`
	Message    json.RawMessage `json:"message"`
}

// Handler answers the CLI's control requests. A nil function means the host does not handle that subtype, and the request is answered with an error at once, as the SDK answers a subtype it has no callback for, so the CLI never waits on a host that will not reply. Each request is handled on its own goroutine; ctx ends when the CLI withdraws the request or the stream ends, and then nothing is sent.
type Handler struct {
	CanUseTool func(ctx context.Context, req PermissionRequest) (Permission, error)
	// Hook returns the hook's output (sdk.d.ts SyncHookJSONOutput).
	Hook func(ctx context.Context, req HookCallback) (json.RawMessage, error)
	// MCP returns the server's JSON-RPC reply.
	MCP func(ctx context.Context, req MCPMessage) (json.RawMessage, error)
	// Observe, when set, sees every line but keep_alive on the reading goroutine, in stream order, before a control request on it is handed to CanUseTool: what the CLI reported before a request (its permission mode) is known when the request is decided, which a reader of Next, behind the queue, cannot promise.
	Observe func(Line)
	// Name appears in the refusal of an unhandled subtype ("orbit does not handle hook_callback requests in a build"); "orbit" when empty.
	Name string
}

// answer computes the response line for one request; nil sends nothing.
func (h Handler) answer(ctx context.Context, id string, raw json.RawMessage) []byte {
	var head struct {
		Subtype string `json:"subtype"`
	}
	_ = json.Unmarshal(raw, &head)
	var body any
	var err error
	switch {
	case head.Subtype == SubtypeCanUseTool && h.CanUseTool != nil:
		req := PermissionRequest{}
		if err = json.Unmarshal(raw, &req); err == nil {
			req.RequestID = id
			if len(req.Input) == 0 || req.Input[0] != '{' {
				req.Input = json.RawMessage(`{}`)
			}
			var p Permission
			if p, err = h.CanUseTool(ctx, req); err == nil {
				body = p.body(req)
			}
		}
	case head.Subtype == SubtypeHookCallback && h.Hook != nil:
		req := HookCallback{}
		if err = json.Unmarshal(raw, &req); err == nil {
			req.RequestID = id
			var out json.RawMessage
			if out, err = h.Hook(ctx, req); err == nil {
				body = out
			}
		}
	case head.Subtype == SubtypeMCPMessage && h.MCP != nil:
		req := MCPMessage{}
		if err = json.Unmarshal(raw, &req); err == nil {
			req.RequestID = id
			var out json.RawMessage
			if out, err = h.MCP(ctx, req); err == nil {
				body = map[string]any{"mcp_response": out}
			}
		}
	default:
		name := h.Name
		if name == "" {
			name = "orbit"
		}
		err = fmt.Errorf("%s does not handle %s requests", name, head.Subtype)
	}
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return errorLine(id, err.Error())
	}
	return successLine(id, body)
}

// Recorded makes every permission decision durable before it is answered: decide makes the decision, record stores it, and only a stored decision is sent. A failed record turns an allow into a deny (an allow nobody can account for is never given) and keeps a deny; a nil record allows nothing. An error from decide is a deny too, recorded like any other.
func Recorded(decide func(context.Context, PermissionRequest) Permission, record func(PermissionRequest, Permission) error) func(context.Context, PermissionRequest) (Permission, error) {
	return func(ctx context.Context, req PermissionRequest) (Permission, error) {
		if record == nil {
			return Permission{Message: "orbit cannot record decisions, so it allows nothing"}, nil
		}
		p := decide(ctx, req)
		if err := record(req, p); err != nil && p.Allow {
			return Permission{Message: "orbit could not record this decision, so it is denied; try again"}, nil
		}
		return p, nil
	}
}

// ControlError is the CLI's error answer to a request the host sent.
type ControlError struct {
	Subtype string
	Message string
}

func (e *ControlError) Error() string {
	return fmt.Sprintf("claude refused %s: %s", e.Subtype, e.Message)
}

// ErrEnded is returned once the CLI's stream has ended: by Next after the last line, and by a request the CLI never answered.
var ErrEnded = errors.New("the CLI's stream ended")
