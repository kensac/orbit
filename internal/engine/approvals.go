package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// Approvals (docs/api.md, Approvals): what a chat run's level leaves as "ask" is put to its person as an approval, recorded before it is answered. Nobody attends an unattended run, so its requests are denied at once; a request nobody answers in time is denied, never allowed.

// ApprovalTimeout is how long a request waits for its person, at most: the run's own deadline comes first.
const ApprovalTimeout = 10 * time.Minute

// approvalPoll is how often a waiting request reads its approval: the person may answer on any instance.
const approvalPoll = time.Second

// exitPlanMode is the CLI's tool for presenting a plan; its ask is a plan approval.
const exitPlanMode = "ExitPlanMode"

// currentLevel is a running turn's level, as watchLevel last saw it.
type currentLevel struct {
	mu    sync.Mutex
	level string
}

func (c *currentLevel) set(l string) {
	c.mu.Lock()
	c.level = l
	c.mu.Unlock()
}

func (c *currentLevel) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.level
}

func deny(msg string) claudeproto.Permission { return claudeproto.Permission{Message: msg} }

// approvalEvent is orbit.approval: an approval opened or ended, for the journal's approval item.
func approvalEvent(a store.Approval, req claudeproto.PermissionRequest) map[string]any {
	m := map[string]any{"request_id": req.RequestID, "approval_id": a.ID, "kind": a.Kind, "tool": a.Tool, "level": a.Level,
		"state": a.State, "expires_at": a.ExpiresAt}
	if a.Scope != nil {
		m["scope"] = *a.Scope
	}
	if a.DecidedBy != nil {
		m["decided_by"] = *a.DecidedBy
	}
	if a.Message != "" {
		m["message"] = a.Message
	}
	return m
}

// approvalPrompt answers a chat run's permission requests: denied at once in a run nobody attends, allowed by an earlier grant for the conversation, else put to the person and awaited until deadline or ApprovalTimeout.
func (e *Engine) approvalPrompt(a *spec.Agent, c *store.ClaimedRun, lvl *currentLevel, deadline time.Time, emit func(string, any)) runner.Prompt {
	attended := Attended(c.Trigger)
	return func(ctx context.Context, req claudeproto.PermissionRequest) claudeproto.Permission {
		if !attended {
			return deny("nobody is attending this run, so orbit denies what it would ask a person; it runs only what the agent's level allows")
		}
		level := lvl.get()
		if level == spec.LevelLocked {
			return deny("this conversation is locked: only what the agent allows by name runs")
		}
		kind := store.ApprovalKindTool
		if req.Tool == exitPlanMode {
			kind = store.ApprovalKindPlan
		}
		bg := context.WithoutCancel(ctx)
		if kind == store.ApprovalKindTool && !req.RequiresUserInteraction {
			if grants, err := e.Store.ConversationGrants(bg, c.ConversationID); err == nil {
				if g := matchGrant(grants, req); g != nil {
					emit("orbit.approval", map[string]any{"request_id": req.RequestID, "kind": kind, "tool": req.Tool, "level": level,
						"state": store.ApprovalAllowed, "scope": store.ScopeConversation, "grant": g.ID})
					return claudeproto.Permission{Allow: true}
				}
			}
		}
		wait := ApprovalTimeout
		if e.ApprovalTimeout > 0 {
			wait = e.ApprovalTimeout
		}
		expires := time.Now().Add(wait)
		if !deadline.IsZero() && deadline.Before(expires) {
			expires = deadline
		}
		ap, err := e.Store.CreateApproval(bg, store.NewApproval{
			RunID: c.ID, Attempt: c.Attempt, ConversationID: c.ConversationID, Agent: a.Name, RequestID: req.RequestID,
			Kind: kind, Tool: req.Tool, Level: level, Input: req.Input, MatchedRule: req.MatchedRule, Suggestions: req.Suggestions,
			DecisionReason: req.DecisionReason, DecisionReasonType: req.DecisionReasonType, BlockedPath: req.BlockedPath,
			Title: req.Title, Description: req.Description,
			ConversationGrant: kind == store.ApprovalKindTool && !req.SuppressAlwaysAllow && !req.RequiresUserInteraction,
			ExpiresAt:         expires,
		})
		if err != nil {
			return deny("orbit could not record this request for its person, so it is denied")
		}
		emit("orbit.approval", approvalEvent(ap, req))
		end := func(state, msg string) claudeproto.Permission {
			got, err := e.Store.CloseApproval(bg, ap.ID, state, msg)
			if err == nil && got.State == store.ApprovalAllowed {
				// The person's answer landed first.
				emit("orbit.approval", approvalEvent(got, req))
				return claudeproto.Permission{Allow: true}
			}
			if err == nil {
				ap = got
			}
			emit("orbit.approval", approvalEvent(ap, req))
			return deny(msg)
		}
		t := time.NewTicker(approvalPoll)
		defer t.Stop()
		timeout := time.NewTimer(time.Until(expires))
		defer timeout.Stop()
		for {
			select {
			case <-ctx.Done():
				return end(store.ApprovalWithdrawn, "the request was withdrawn before its person answered")
			case <-timeout.C:
				return end(store.ApprovalExpired, "nobody answered in time, so orbit denies it")
			case <-t.C:
				if lvl.get() == spec.LevelLocked {
					return end(store.ApprovalDenied, "the conversation was locked while this waited")
				}
				cur, err := e.Store.GetApproval(bg, ap.ID)
				if err != nil {
					continue
				}
				switch cur.State {
				case store.ApprovalAllowed:
					emit("orbit.approval", approvalEvent(cur, req))
					return claudeproto.Permission{Allow: true}
				case store.ApprovalDenied:
					emit("orbit.approval", approvalEvent(cur, req))
					msg := cur.Message
					if msg == "" {
						msg = "your person denied this"
					}
					return deny(msg)
				case store.ApprovalExpired:
					return end(store.ApprovalExpired, "nobody answered in time, so orbit denies it")
				}
			}
		}
	}
}

// matchGrant is the grant that answers req: one for the same tool whose input is the same, or whose CLI suggestion (a rule or a directory it would add) is one req offers too. A grant never reaches further than the CLI's own suggestion the person accepted with it.
func matchGrant(grants []store.Approval, req claudeproto.PermissionRequest) *store.Approval {
	want := suggestionKeys(req.Suggestions)
	in := canonical(req.Input)
	for i := range grants {
		g := &grants[i]
		if g.Tool != req.Tool {
			continue
		}
		if in != "" && canonical(g.Input) == in {
			return g
		}
		for k := range suggestionKeys(g.Suggestions) {
			if want[k] {
				return g
			}
		}
	}
	return nil
}

// suggestionKeys are a request's grantable suggestions, each without its destination: allow rules and directories. A mode suggestion is never one: levels are orbit's.
func suggestionKeys(raw json.RawMessage) map[string]bool {
	var list []map[string]any
	_ = json.Unmarshal(raw, &list)
	out := map[string]bool{}
	for _, s := range list {
		switch s["type"] {
		case "addRules":
			if s["behavior"] != "allow" {
				continue
			}
		case "addDirectories":
		default:
			continue
		}
		delete(s, "destination")
		b, _ := json.Marshal(s)
		out[string(b)] = true
	}
	return out
}

func canonical(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	b, _ := json.Marshal(v)
	if bytes.Equal(b, []byte("{}")) {
		return ""
	}
	return string(b)
}
