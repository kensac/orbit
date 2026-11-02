// Package status derives what an agent is doing (working, waiting on its
// person, done, errored) from two producers: the Claude Code hook events
// `orbit hook` posts while a run's turn is live, and the run's own row once
// orbit has recorded it.
//
// Precedence: while a run is running, the hooks speak for it wherever they
// have reported for its current attempt; the run's status fills in where
// they have not (another backend, hooks off, the time before the first
// event). Once the run is final, its row decides: a turn the hooks saw end
// can still fail orbit's contract, and a finished run whose questions or
// actions are open is waiting on its person, which no hook can know.
package status

import (
	"encoding/json"
	"time"
)

// State is one agent status.
type State string

// The states, in the order a run normally passes through them.
const (
	// Idle: nothing has run, or the latest run was cancelled before it started.
	Idle State = "idle"
	// Queued: a run is waiting for an executor.
	Queued State = "queued"
	// Working: the agent's turn is in progress.
	Working State = "working"
	// Waiting: the agent is blocked on its person (Kind says on what).
	Waiting State = "waiting"
	// Done: the latest turn ended and nothing waits on the person.
	Done State = "done"
	// Errored: the latest turn or run failed (Detail says why).
	Errored State = "errored"
)

// Kind is what a waiting agent waits for.
type Kind string

// The kinds.
const (
	// Permission: a tool call is held for a person's permission.
	Permission Kind = "permission"
	// Question: the agent asked its person something (an AskUserQuestion
	// call mid-turn, or an open question of a finished run).
	Question Kind = "question"
	// Elicitation: an MCP server asked the person for input.
	Elicitation Kind = "elicitation"
	// Approval: a finished run's action waits for a person's decision.
	Approval Kind = "approval"
)

// The Claude Code hook events orbit configures for a run. Each runs `orbit
// hook`, which posts it to the run's listener. TeammateIdle and the
// agent-team events are left out: orbit never starts a team.
var HookEvents = []string{
	"SessionStart", "UserPromptSubmit",
	"PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest",
	"Notification", "Elicitation", "ElicitationResult",
	"SubagentStart", "SubagentStop", "PreCompact", "PostCompact",
	"Stop", "StopFailure", "SessionEnd",
}

// ToolEvents are the events whose hook entry takes a tool matcher.
var ToolEvents = map[string]bool{
	"PreToolUse": true, "PostToolUse": true, "PostToolUseFailure": true, "PermissionRequest": true,
}

// Event is what the hook posts: the fields of a hook input status needs,
// and when the hook ran. The rest of the input (tool inputs and responses,
// the prompt, the last message) stays out: it is in the run's stream
// already, and a line this small is one atomic append to the spool.
type Event struct {
	Name      string    `json:"event"`
	At        time.Time `json:"at"`
	SessionID string    `json:"session_id,omitempty"`
	// PromptID names the turn; the CLI sets it from UserPromptSubmit on
	// (seen on 2.1.316: SessionStart has none).
	PromptID  string `json:"prompt_id,omitempty"`
	Tool      string `json:"tool,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	// AgentID is set on events from a subagent's own work.
	AgentID   string `json:"agent_id,omitempty"`
	AgentType string `json:"agent_type,omitempty"`
	// Notification is a Notification event's notification_type.
	Notification string `json:"notification,omitempty"`
	// Message is a Notification's message.
	Message string `json:"message,omitempty"`
	// Error is StopFailure's error code (model_not_found, rate_limit, ...).
	Error string `json:"error,omitempty"`
	// Reason is SessionEnd's reason; Source is SessionStart's source.
	Reason string `json:"reason,omitempty"`
	Source string `json:"source,omitempty"`
}

// maxField bounds each text field of an Event, so a posted line stays
// small (well under the 4 KiB PIPE_BUF a single append is atomic within).
const maxField = 300

// FromHook reads the fields of a Claude Code hook input (sdk.d.ts
// BaseHookInput and the per-event inputs) into an Event stamped at.
func FromHook(input []byte, at time.Time) (Event, error) {
	var in struct {
		Name         string `json:"hook_event_name"`
		SessionID    string `json:"session_id"`
		PromptID     string `json:"prompt_id"`
		Tool         string `json:"tool_name"`
		ToolUseID    string `json:"tool_use_id"`
		AgentID      string `json:"agent_id"`
		AgentType    string `json:"agent_type"`
		Notification string `json:"notification_type"`
		Message      string `json:"message"`
		Error        any    `json:"error"`
		Reason       string `json:"reason"`
		Source       string `json:"source"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return Event{}, err
	}
	ev := Event{Name: in.Name, At: at.UTC(), SessionID: in.SessionID, PromptID: in.PromptID,
		Tool: in.Tool, ToolUseID: in.ToolUseID, AgentID: in.AgentID, AgentType: in.AgentType,
		Notification: in.Notification, Message: in.Message, Reason: in.Reason, Source: in.Source}
	switch e := in.Error.(type) {
	case string:
		ev.Error = e
	case nil:
	default:
		b, _ := json.Marshal(e)
		ev.Error = string(b)
	}
	for _, f := range []*string{&ev.SessionID, &ev.PromptID, &ev.Tool, &ev.ToolUseID, &ev.AgentID,
		&ev.AgentType, &ev.Notification, &ev.Message, &ev.Error, &ev.Reason, &ev.Source} {
		*f = clip(*f, maxField)
	}
	return ev, nil
}

// clip cuts s to at most n bytes at a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
