package store_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// answer claims the next run (in conv), reads its messages, writes an
// assistant line naming uuid and succeeds in session.
func answer(t *testing.T, s *store.Store, conv, report, session, uuid string) string {
	t.Helper()
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil || c.ConversationID != conv {
		t.Fatalf("claim %+v %v, want conversation %s", c, err, conv)
	}
	if err := s.SetRunner(ctx, c.ID, c.Attempt, "claude-code", "", "sha-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartRunContext(ctx, c.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, c.ID, c.Attempt, 1, "assistant", []byte(`{"type":"assistant","uuid":"`+uuid+`","message":{"content":[{"type":"text","text":"`+report+`"}]}}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutWorkspace(ctx, conv, c.ID, c.Attempt, 1, []byte(report)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: c.Attempt, Status: "succeeded", Report: report, SessionID: session},
		store.Outcome{}); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func TestAForkHoldsTheThreadUpToItsMessage(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	conv, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Body: "Remember APPLE", Author: "kanishk", Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	p := conv.ConversationID
	model, effort := "sonnet", "high"
	if err := s.UpdateConversation(ctx, p, store.ConversationUpdate{Model: &model, Effort: &effort}); err != nil {
		t.Fatal(err)
	}
	r1 := answer(t, s, p, "Noted APPLE", "live-1", "uuid-1")
	second, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: "Remember BANANA", Author: "kanishk", Wake: true, ConversationID: p})
	if err != nil {
		t.Fatal(err)
	}
	// The second run continued the same session in a live process.
	answer(t, s, p, "Noted BANANA", "live-1", "uuid-2")

	f, _, err := s.ForkConversation(ctx, store.ForkRequest{ConversationID: p, MessageID: conv.MessageID, ClientID: "f1"})
	if err != nil {
		t.Fatal(err)
	}
	if f.ForkedFrom == nil || f.ForkedFrom.ConversationID != p || f.ForkedFrom.MessageID != conv.MessageID ||
		f.Title != "Remember APPLE (fork)" || f.Agent != "a" || *f.Model != "sonnet" || *f.Effort != "high" {
		t.Fatalf("fork = %+v", f)
	}
	if again, _, err := s.ForkConversation(ctx, store.ForkRequest{ConversationID: p, MessageID: conv.MessageID, ClientID: "f1"}); err != nil || again.ID != f.ID {
		t.Fatalf("retried fork = %+v %v", again, err)
	}
	seedS, err := s.ForkSession(ctx, f.ID)
	if err != nil || seedS == nil || seedS.SessionID != "live-1" || seedS.ResumeAt != "uuid-1" || seedS.RunID != r1 || seedS.Parent != p {
		t.Fatalf("seed = %+v %v", seedS, err)
	}
	w, err := s.LatestWorkspace(ctx, f.ID)
	if err != nil || string(w.Archive) != "Noted APPLE" {
		t.Fatalf("workspace = %+v %v, want the one as of the first answer", w, err)
	}

	// A run in the fork is told the parent's thread up to the fork point,
	// then the fork's own.
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: "What do you remember?", Author: "kanishk", Wake: true, ConversationID: f.ID}); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil || c.ConversationID != f.ID {
		t.Fatalf("claim = %+v %v", c, err)
	}
	items, err := s.Conversation(ctx, "a", c.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, it := range items {
		bodies = append(bodies, it.Body)
	}
	if got := strings.Join(bodies, "|"); got != "Noted APPLE|Remember APPLE" {
		t.Fatalf("fork thread = %s", got)
	}
	if _, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: c.Attempt, Status: "succeeded", Report: "APPLE"}, store.Outcome{}); err != nil {
		t.Fatal(err)
	}
	// Once the fork has run, its own sessions follow.
	if seedS, err := s.ForkSession(ctx, f.ID); err != nil || seedS != nil {
		t.Fatalf("seed after a run = %+v %v", seedS, err)
	}

	// A fork of an unanswered message holds it, and no session.
	waiting, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: "Remember CHERRY", Author: "kanishk", ConversationID: p})
	if err != nil {
		t.Fatal(err)
	}
	g, _, err := s.ForkConversation(ctx, store.ForkRequest{ConversationID: p, MessageID: waiting.MessageID})
	if err != nil {
		t.Fatal(err)
	}
	if seedS, err := s.ForkSession(ctx, g.ID); err != nil || seedS != nil {
		t.Fatalf("seed of an unanswered fork = %+v %v", seedS, err)
	}
	// A message whose reply is being written cannot be forked yet; one of
	// another conversation is not found.
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: "sha-a", Trigger: "manual", ConversationID: p}); err != nil {
		t.Fatal(err)
	}
	busy, err := s.ClaimRun(ctx, "w")
	if err != nil || busy == nil {
		t.Fatal(busy, err)
	}
	if _, err := s.StartRunContext(ctx, busy.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ForkConversation(ctx, store.ForkRequest{ConversationID: p, MessageID: waiting.MessageID}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("fork while answering = %v", err)
	}
	if _, _, err := s.ForkConversation(ctx, store.ForkRequest{ConversationID: f.ID, MessageID: second.MessageID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fork at another conversation's message = %v", err)
	}
}
