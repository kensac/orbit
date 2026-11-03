package store_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestApprovalsAreDecidedOnce(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	run := manual(t, s, "a")
	var conv string
	if err := s.Pool.QueryRow(ctx, `SELECT conversation_id::text FROM runs WHERE id = $1`, run).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	n := store.NewApproval{RunID: run, Attempt: 1, ConversationID: conv, Agent: "a", RequestID: "q1", Kind: store.ApprovalKindTool,
		Tool: "Bash", Level: "ask", Input: json.RawMessage(`{"command":"npm test"}`), DecisionReason: "runs a command",
		Suggestions:       json.RawMessage(`[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"npm test:*"}],"behavior":"allow","destination":"localSettings"}]`),
		ConversationGrant: true, ExpiresAt: time.Now().Add(time.Hour)}
	a, err := s.CreateApproval(ctx, n)
	if err != nil || a.State != store.ApprovalPending || a.DecisionReason != "runs a command" || a.MatchedRule != nil {
		t.Fatalf("create = %+v %v", a, err)
	}
	if again, err := s.CreateApproval(ctx, n); err != nil || again.ID != a.ID {
		t.Fatalf("a repeated request is the same row: %v %v", again.ID, err)
	}
	if pending, _ := s.ListApprovals(ctx, conv, store.ApprovalPending, 0); len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}
	got, err := s.DecideApproval(ctx, a.ID, true, store.ScopeConversation, "go ahead", "kanishk")
	if err != nil || got.State != store.ApprovalAllowed || *got.Scope != store.ScopeConversation || *got.DecidedBy != "kanishk" || got.DecidedAt == nil {
		t.Fatalf("decide = %+v %v", got, err)
	}
	if _, err := s.DecideApproval(ctx, a.ID, false, store.ScopeOnce, "", "kanishk"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second decision = %v", err)
	}
	if closed, err := s.CloseApproval(ctx, a.ID, store.ApprovalExpired, "late"); err != nil || closed.State != store.ApprovalAllowed {
		t.Fatalf("closing a decided approval keeps the decision: %+v %v", closed, err)
	}
	if g, err := s.ConversationGrants(ctx, conv); err != nil || len(g) != 1 || g[0].ID != a.ID {
		t.Fatalf("grants = %v %v", g, err)
	}

	// A plan, or an ask the CLI marks as granting more than itself, is allowed once at most.
	n.RequestID, n.Kind, n.ConversationGrant = "q2", store.ApprovalKindPlan, false
	plan, _ := s.CreateApproval(ctx, n)
	if _, err := s.DecideApproval(ctx, plan.ID, true, store.ScopeConversation, "", "kanishk"); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a plan for the conversation = %v", err)
	}
	// Past its time an approval reads expired and cannot be allowed.
	n.RequestID, n.ExpiresAt = "q3", time.Now().Add(-time.Second)
	old, _ := s.CreateApproval(ctx, n)
	if old.State != store.ApprovalExpired {
		t.Fatalf("an approval past its time reads %s", old.State)
	}
	if _, err := s.DecideApproval(ctx, old.ID, true, store.ScopeOnce, "", "kanishk"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("allowing an expired approval = %v", err)
	}
	if exp, _ := s.ListApprovals(ctx, conv, store.ApprovalExpired, 0); len(exp) != 1 || exp[0].ID != old.ID {
		t.Fatalf("expired = %v", exp)
	}
	if all, _ := s.ListApprovals(ctx, conv, "", 0); len(all) != 3 || all[0].ID != old.ID {
		t.Fatalf("all = %d", len(all))
	}
	if _, err := s.GetApproval(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get bad id = %v", err)
	}
}
