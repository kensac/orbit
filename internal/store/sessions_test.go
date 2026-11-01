package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// finish claims the oldest due run of agent and finishes it.
func finish(t *testing.T, s *store.Store, status, session string, mode string, parent string, cost float64) string {
	t.Helper()
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if mode != "" {
		if err := s.SetSession(ctx, c.ID, mode, parent, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FinishRun(ctx, c.ID, store.RunResult{Status: status, SessionID: session, SessionCostUSD: &cost}); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func TestLatestSessionReportsTheLatestFinishedRun(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	if p, err := s.LatestSession(ctx, defaultConv(t, s, "a")); err != nil || p != nil {
		t.Fatalf("no runs: %+v %v", p, err)
	}
	manual(t, s, "a")
	first := finish(t, s, "succeeded", "s1", "fresh", "", 0.5)
	p, err := s.LatestSession(ctx, defaultConv(t, s, "a"))
	if err != nil || p.RunID != first || p.SessionID != "s1" || p.Status != "succeeded" || *p.CostUSD != 0.5 || p.Tainted {
		t.Fatalf("latest = %+v %v", p, err)
	}
	// A live turn on s1 that failed taints it: the live process wrote the
	// failed turn into s1's transcript.
	manual(t, s, "a")
	finish(t, s, "failed", "", "live", "s1", 0)
	if p, _ = s.LatestSession(ctx, defaultConv(t, s, "a")); p.Status != "failed" || p.SessionID != "" {
		t.Fatalf("after a failure = %+v", p)
	}
	manual(t, s, "a")
	finish(t, s, "succeeded", "s2", "fresh", "", 0.1)
	if p, _ = s.LatestSession(ctx, defaultConv(t, s, "a")); p.SessionID != "s2" || p.Tainted {
		t.Fatalf("a fresh session after the failure = %+v", p)
	}
	// A live turn requeued mid-turn (shutdown) leaves the run queued with
	// its session recorded: that, too, taints the session it continued.
	manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	if err := s.SetSession(ctx, c.ID, "live", "s2", 2); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueRun(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if p, _ = s.LatestSession(ctx, defaultConv(t, s, "a")); p.SessionID != "s2" || !p.Tainted {
		t.Fatalf("an interrupted live turn must taint its session: %+v", p)
	}
}

func TestSetSessionNeedsARunningRun(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := manual(t, s, "a")
	if err := s.SetSession(ctx, id, "fresh", "", 1); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestContinuationsWaitDeduplicateAndChain(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	manual(t, s, "a")
	from := finish(t, s, "succeeded", "", "", "", 0)

	c := store.Continuation{Agent: "a", FromRun: from, After: time.Hour, Note: "check the export", MaxChain: 2}
	id, queued, err := s.EnqueueContinuation(ctx, c)
	if err != nil || !queued || id == "" {
		t.Fatalf("enqueue: %q %v %v", id, queued, err)
	}
	// One waits per agent; a second request names the one waiting.
	again, queued, err := s.EnqueueContinuation(ctx, store.Continuation{Agent: "a", FromRun: from, After: time.Minute, Note: "other", MaxChain: 2})
	if err != nil || queued || again != id {
		t.Fatalf("second enqueue: %q %v %v, want %q not queued", again, queued, err, id)
	}
	// Not claimed before run_after.
	if got, err := s.ClaimRun(ctx, "w"); err != nil || got != nil {
		t.Fatalf("claimed a continuation before it was due: %+v %v", got, err)
	}
	run, _ := s.GetRun(ctx, id)
	if run.Trigger != "continue" || run.Chain != 1 || run.Prompt != "check the export" || *run.Continues != from ||
		run.RunAfter.Before(time.Now().Add(59*time.Minute)) {
		t.Fatalf("continuation = %+v", run)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET run_after = now() WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	got, err := s.ClaimRun(ctx, "w")
	if err != nil || got == nil || got.ID != id || got.Chain != 1 || got.Trigger != "continue" {
		t.Fatalf("claim once due = %+v %v", got, err)
	}
	// A continuation asked for by a continuation extends the chain, up to
	// MaxChain.
	if err := s.FinishRun(ctx, id, store.RunResult{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	second, _, err := s.EnqueueContinuation(ctx, store.Continuation{Agent: "a", FromRun: id, After: time.Minute, Note: "n", MaxChain: 2})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRun(ctx, second); r.Chain != 2 {
		t.Fatalf("chain = %d, want 2", r.Chain)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET run_after = now() WHERE id = $1`, second); err != nil {
		t.Fatal(err)
	}
	finish(t, s, "succeeded", "", "", "", 0)
	if _, _, err := s.EnqueueContinuation(ctx, store.Continuation{Agent: "a", FromRun: second, After: time.Minute, Note: "n", MaxChain: 2}); !errors.Is(err, store.ErrChainLimit) {
		t.Fatalf("err = %v, want the chain limit", err)
	}
}

func TestConversationIsTheThreadBeforeThisRun(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	if _, err := s.SendToAgent(ctx, "a", "how much on food?", "k", nil); err != nil {
		t.Fatal(err)
	}
	manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	if _, err := s.StartRunContext(ctx, c.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Status: "succeeded", Summary: "$412", Report: "Food was $412."},
		store.Outcome{Questions: []string{"Include restaurants?"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SendToAgent(ctx, "a", "yes, include them", "k", nil); err != nil {
		t.Fatal(err)
	}
	manual(t, s, "a")
	next, _ := s.ClaimRun(ctx, "w")
	if _, err := s.StartRunContext(ctx, next.ID, "a"); err != nil {
		t.Fatal(err)
	}
	items, err := s.Conversation(ctx, "a", next.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first; the message the current run consumed is not part of
	// the history (the prompt shows it as new).
	var got []string
	for _, it := range items {
		got = append(got, it.Kind+":"+it.Body)
	}
	want := []string{"question:Include restaurants?", "report:Food was $412.", "message:how much on food?"}
	if len(got) != len(want) {
		t.Fatalf("items = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("items = %v, want %v", got, want)
		}
	}
	if items[1].Summary != "$412" || items[1].RunID != c.ID {
		t.Fatalf("report item = %+v", items[1])
	}
}
