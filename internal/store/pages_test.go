package store_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestConversationPageIsOnlyVisibleAfterExecutionAndInItsConversation(t *testing.T) {
	s := testdb.New(t)
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"chat": {SHA: "sha-chat", JSON: []byte(`{"name":"chat","chat":true}`), Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	runID, err := s.EnqueueRun(ctx, store.NewRun{Agent: "chat", SpecSHA: "sha-chat", Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.ClaimRun(ctx, "test")
	if err != nil || claim == nil {
		t.Fatalf("claim = %+v, %v", claim, err)
	}
	convID, err := s.DefaultConversationID(ctx, "chat")
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"title":"A page","html":"<main>content</main>"}`)
	pageID, err := s.StartActionNow(ctx, claim.ID, claim.Attempt, "chat", store.ProposedAction{Type: "page:publish", Params: params, Rule: "approve", InstructedBy: "person"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConversationPage(ctx, convID, pageID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("executing page visible: %v", err)
	}
	if count, err := s.PageCountForRun(ctx, runID, pageID); err != nil || count != 0 {
		t.Fatalf("page count while executing = %d, %v", count, err)
	}
	if err := s.FinishAction(ctx, pageID, json.RawMessage(`{"page_id":"id","title":"A page"}`), nil); err != nil {
		t.Fatal(err)
	}
	p, err := s.ConversationPage(ctx, convID, pageID)
	if err != nil || p.HTML != "<main>content</main>" || p.Title != "A page" || p.ConversationID != convID {
		t.Fatalf("page = %+v, %v", p, err)
	}
	if _, err := s.ConversationPage(ctx, "00000000-0000-0000-0000-000000000001", pageID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-conversation lookup = %v", err)
	}
	if count, err := s.PageCountForRun(ctx, runID, pageID); err != nil || count != 0 {
		t.Fatalf("page count = %d, %v", count, err)
	}
	if count, err := s.PageCountForRun(ctx, runID, "00000000-0000-0000-0000-000000000001"); err != nil || count != 1 {
		t.Fatalf("other page count = %d, %v", count, err)
	}
}
