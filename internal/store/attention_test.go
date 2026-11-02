package store_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func kinds(t *testing.T, s *store.Store) map[string]store.Attention {
	t.Helper()
	list, err := s.ListAttention(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.Attention{}
	for _, a := range list {
		out[a.Kind] = a
	}
	if len(out) != len(list) {
		t.Fatalf("two open rows of one kind: %+v", list)
	}
	return out
}

func TestAttentionOpensAndClosesWithItsThing(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	ids, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: 1, Status: "succeeded", Report: "spent $2,700"}, store.Outcome{
		Questions: []string{"Is the Enterprise charge deductible?"},
		Actions: []store.ProposedAction{
			{Type: "webhook:pay", Params: json.RawMessage(`{"amount":2700}`), Reason: "rent is due", Rule: "approve"},
			{Type: "notify", Params: json.RawMessage(`{}`), Rule: "auto"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := kinds(t, s)
	ap, q := got[store.AttentionApproval], got[store.AttentionQuestion]
	if len(got) != 2 || ap.ActionID == nil || *ap.ActionID != ids[0] || q.MessageID == nil {
		t.Fatalf("open = %+v, want the pending approval and the question", got)
	}
	if ap.Title != "a wants to webhook:pay" || q.Title != "a has a question" {
		t.Fatalf("titles %q, %q", ap.Title, q.Title)
	}
	// Titles never carry what the run wrote; the detail is for the inbox.
	if ap.Detail != "rent is due" || q.Detail != "Is the Enterprise charge deductible?" || !ap.Push || ap.ConversationID == "" {
		t.Fatalf("approval %+v question %+v", ap, q)
	}

	if err := s.DecideAction(ctx, ids[0], true, "me"); err != nil {
		t.Fatal(err)
	}
	if got := kinds(t, s); len(got) != 1 || got[store.AttentionQuestion].ID == "" {
		t.Fatalf("after approving: %+v, want only the question", got)
	}
	if _, err := s.SendToAgent(ctx, "a", "yes", "me", nil); err != nil {
		t.Fatal(err)
	}
	if got := kinds(t, s); len(got) != 0 {
		t.Fatalf("after answering: %+v, want nothing", got)
	}
}

func TestAttentionForFailuresAndAttendedRuns(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := manual(t, s, "a")
	_, _ = s.ClaimRun(ctx, "w")
	if err := s.FinishRun(ctx, id, store.RunResult{Attempt: 1, Status: "failed", Error: "source sure: 502"}); err != nil {
		t.Fatal(err)
	}
	f := kinds(t, s)[store.AttentionFailure]
	if f.RunID != id || f.Title != "a run failed" || f.Detail != "source sure: 502" || !f.Push {
		t.Fatalf("failure = %+v", f)
	}
	if err := s.DismissAttention(ctx, f.ID, "me"); err != nil {
		t.Fatal(err)
	}
	if err := s.DismissAttention(ctx, f.ID, "me"); err != nil {
		t.Fatalf("dismissing twice = %v, want nil", err)
	}
	if err := s.DismissAttention(ctx, "00000000-0000-0000-0000-000000000000", "me"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
	if got := kinds(t, s); len(got) != 0 {
		t.Fatalf("dismissed failure still open: %+v", got)
	}

	// A run answering a person's message is in the inbox but not pushed.
	if _, err := s.SendToAgent(ctx, "a", "check again", "me", nil); err != nil {
		t.Fatal(err)
	}
	mid, err := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: "sha-a", Trigger: "message"})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.ClaimRun(ctx, "w")
	if err := s.FinishRun(ctx, mid, store.RunResult{Attempt: 1, Status: "failed", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	f = kinds(t, s)[store.AttentionFailure]
	if f.RunID != mid || f.Push {
		t.Fatalf("attended failure = %+v, want listed with push false", f)
	}
	if items, err := s.ClaimPush(ctx, 0, time.Hour); err != nil || len(items) != 0 {
		t.Fatalf("claimed %+v %v, want nothing to push", items, err)
	}

	// A later success in the conversation closes a failure.
	manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	if _, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{}); err != nil {
		t.Fatal(err)
	}
	if got := kinds(t, s); len(got) != 0 {
		t.Fatalf("failure open after a later success: %+v", got)
	}
}

func TestClaimPushHoldsTheGapAndSkipsClosed(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	fail := func() string {
		id := manual(t, s, "a")
		_, _ = s.ClaimRun(ctx, "w")
		if err := s.FinishRun(ctx, id, store.RunResult{Attempt: 1, Status: "failed", Error: "x"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := fail()
	items, err := s.ClaimPush(ctx, time.Hour, 24*time.Hour)
	if err != nil || len(items) != 1 || items[0].RunID != first {
		t.Fatalf("first claim = %+v %v", items, err)
	}
	// Inside the gap nothing goes out; the rows wait and fold together.
	fail()
	fail()
	if items, _ := s.ClaimPush(ctx, time.Hour, 24*time.Hour); len(items) != 0 {
		t.Fatalf("claimed inside the gap: %+v", items)
	}
	items, _ = s.ClaimPush(ctx, 0, 24*time.Hour)
	if len(items) != 2 || !items[0].CreatedAt.Before(items[1].CreatedAt) {
		t.Fatalf("after the gap = %+v, want both, oldest first", items)
	}
	if again, _ := s.ClaimPush(ctx, 0, 24*time.Hour); len(again) != 0 {
		t.Fatalf("pushed twice: %+v", again)
	}

	// Closed before its turn: marked, not returned.
	manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	ids, _ := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{{Type: "notify", Params: json.RawMessage(`{}`), Rule: "approve"}}})
	_ = s.DecideAction(ctx, ids[0], false, "me")
	if items, _ := s.ClaimPush(ctx, 0, 24*time.Hour); len(items) != 0 {
		t.Fatalf("a decided approval was pushed: %+v", items)
	}
}

func TestPushSubscriptions(t *testing.T) {
	s := testdb.New(t)
	sub := store.PushSubscription{Endpoint: "https://fcm.googleapis.com/fcm/send/x", P256DH: "k", Auth: "a", Author: "kanishk", Label: "Pixel"}
	id, err := s.SavePushSubscription(ctx, sub)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SavePushSubscription(ctx, store.PushSubscription{Endpoint: "e"}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("missing keys = %v", err)
	}
	if err := s.PushResult(ctx, id, errors.New("gone"), true); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListPushSubscriptions(ctx, ""); len(list) != 0 {
		t.Fatalf("a gone subscription is still active: %+v", list)
	}
	// The browser subscribes again: same endpoint, same row, revived.
	sub.P256DH = "k2"
	again, err := s.SavePushSubscription(ctx, sub)
	if err != nil || again != id {
		t.Fatalf("resubscribe = %s %v, want %s", again, err, id)
	}
	list, _ := s.ListPushSubscriptions(ctx, "kanishk")
	if len(list) != 1 || list[0].P256DH != "k2" || list[0].Failures != 0 || list[0].Label != "Pixel" {
		t.Fatalf("list = %+v", list)
	}
	if other, _ := s.ListPushSubscriptions(ctx, "someone"); len(other) != 0 {
		t.Fatalf("another author's devices: %+v", other)
	}
	if err := s.PushResult(ctx, id, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokePushSubscription(ctx, sub.Endpoint); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokePushSubscription(ctx, sub.Endpoint); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("revoking twice = %v", err)
	}
}

func TestAttentionForAPlanAwaitingApproval(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	nt := newRequest("")
	nt.Wake = true
	r, _, err := s.CreateTicket(ctx, nt)
	if err != nil {
		t.Fatal(err)
	}
	v1, err := s.ProposePlan(ctx, r.TicketID, r.RunID, "plan one")
	if err != nil {
		t.Fatal(err)
	}
	d := kinds(t, s)[store.AttentionDecision]
	if d.TicketID == nil || *d.TicketID != r.TicketID || d.PlanVersion == nil || *d.PlanVersion != v1 || d.Detail != "Add a flag" {
		t.Fatalf("decision = %+v", d)
	}
	// A newer plan replaces the older one in the inbox.
	v2, _ := s.ProposePlan(ctx, r.TicketID, r.RunID, "plan two")
	if d := kinds(t, s)[store.AttentionDecision]; *d.PlanVersion != v2 {
		t.Fatalf("open plan = %+v, want version %d", d, v2)
	}
	if _, err := s.ApprovePlan(ctx, r.TicketID, v2, "me", ""); err != nil {
		t.Fatal(err)
	}
	if got := kinds(t, s); len(got) != 0 {
		t.Fatalf("after approval: %+v", got)
	}
}

func TestAttentionForADelegatedTasksDecision(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a", "b")
	manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	ids, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{{Type: "delegate", Params: json.RawMessage(`{}`), Rule: "auto"}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Delegate(ctx, store.Delegation{ActionID: ids[0], RunID: c.ID,
		Tasks: []store.NewTask{{Key: "k", Agent: "b", Brief: "book it", Decision: "Spend $50 on the venue?"}}})
	if err != nil || len(out) != 1 || out[0].DecisionID == nil {
		t.Fatalf("delegate = %+v %v", out, err)
	}
	d := kinds(t, s)[store.AttentionDecision]
	if d.ActionID == nil || *d.ActionID != *out[0].DecisionID || d.Title != "a needs a decision" || d.Detail != "Spend $50 on the venue?" {
		t.Fatalf("decision = %+v", d)
	}
	if err := s.DecideAction(ctx, *out[0].DecisionID, true, "me"); err != nil {
		t.Fatal(err)
	}
	if got := kinds(t, s); len(got) != 0 {
		t.Fatalf("after deciding: %+v", got)
	}
}
