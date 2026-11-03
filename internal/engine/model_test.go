package engine_test

import (
	"testing"

	"github.com/kensac/orbit/internal/store"
)

func TestARunUsesItsConversationsModelAndEffort(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harness(t, fr)
	c := claim(t, e, s)
	model, effort := "sonnet", "high"
	if err := s.UpdateConversation(ctx, c.ConversationID, store.ConversationUpdate{Model: &model, Effort: &effort}); err != nil {
		t.Fatal(err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if fr.got.Model != "sonnet" || fr.got.Effort != "high" {
		t.Fatalf("request model %q effort %q", fr.got.Model, fr.got.Effort)
	}
	run, _ := s.GetRun(ctx, c.ID)
	if run.Model == nil || *run.Model != "sonnet" || run.Effort == nil || *run.Effort != "high" {
		t.Fatalf("run recorded model %v effort %v", run.Model, run.Effort)
	}

	// A pair the runner does not take (stored before a runner change) is
	// left out, and the transcript says why.
	c = claim(t, e, s)
	bad := "extreme"
	if err := s.UpdateConversation(ctx, c.ConversationID, store.ConversationUpdate{Effort: &bad}); err != nil {
		t.Fatal(err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if fr.got.Model != "" || fr.got.Effort != "" {
		t.Fatalf("an unusable pair was used: %q %q", fr.got.Model, fr.got.Effort)
	}
	if run, _ = s.GetRun(ctx, c.ID); run.Effort != nil || run.Model == nil || *run.Model != "fake" {
		t.Fatalf("run recorded model %v effort %v", run.Model, run.Effort)
	}
	evs, err := s.ListEvents(ctx, c.ID, c.Attempt, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, ev := range evs {
		found = found || ev.Kind == "orbit.model_skipped"
	}
	if !found {
		t.Fatal("no orbit.model_skipped event")
	}
}
