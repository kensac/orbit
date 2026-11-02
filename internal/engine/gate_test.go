package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/store"
)

// scheduled enqueues and claims a schedule fire at the given minute.
func scheduled(t *testing.T, e *engine.Engine, s *store.Store, minute int) *store.ClaimedRun {
	t.Helper()
	a := e.Current().Config.Agents["finance"]
	idx, at := 0, time.Date(2026, 10, 1, 9, minute, 0, 0, time.UTC)
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "finance", SpecSHA: a.SHA, Trigger: "schedule", TriggerIndex: &idx, ScheduledFor: &at}); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	return c
}

// TestUnchangedInputsSkipTheModel: a scheduled run whose inputs are what
// the last succeeded run judged ends skipped, naming the gate, without a
// backend call or a notification; a person's message still reaches it.
func TestUnchangedInputsSkipTheModel(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, h := harnessWith(t, fr, "gates: [{kind: unchanged}]\n")
	first := scheduled(t, e, s, 0)
	if err := e.Execute(ctx, first); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRun(ctx, first.ID); r.Status != "succeeded" || fr.calls != 1 {
		t.Fatalf("first run = %s, calls %d: nothing judged yet, so it must run", r.Status, fr.calls)
	}
	h.mu.Lock()
	sent := len(h.messages)
	h.mu.Unlock()

	second := scheduled(t, e, s, 1)
	if err := e.Execute(ctx, second); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetRun(ctx, second.ID)
	if r.Status != store.StatusSkipped || r.Gate == nil || *r.Gate != "unchanged" || fr.calls != 1 {
		t.Fatalf("second run = %s gate %v, calls %d", r.Status, r.Gate, fr.calls)
	}
	if !strings.Contains(*r.Summary, first.ID) {
		t.Fatalf("summary %q does not name the run it matched", *r.Summary)
	}
	h.mu.Lock()
	if len(h.messages) != sent {
		t.Fatalf("a skipped run notified: %v", h.messages[sent:])
	}
	h.mu.Unlock()

	// A manual run is someone asking: no gate applies.
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRun(ctx, c.ID); r.Status != "succeeded" || fr.calls != 2 {
		t.Fatalf("manual run = %s, calls %d", r.Status, fr.calls)
	}
}

func TestItemsAboveSkipsAnEmptySource(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, `
transforms:
  - {name: none, source: tx, kind: json-filter, params: {where: [{field: page, op: gt, value: 5}]}, keep_source: true}
gates:
  - {kind: items_above, input: tx, n: 1}
`)
	c := scheduled(t, e, s, 0)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRun(ctx, c.ID); r.Status != "succeeded" || fr.calls != 1 {
		t.Fatalf("tx has 2 records, above 1: run = %s, calls %d", r.Status, fr.calls)
	}
	e2, s2, _ := harnessWith(t, fr, `
transforms:
  - {name: none, source: tx, kind: json-filter, params: {where: [{field: page, op: gt, value: 5}]}}
gates:
  - {kind: items_above, input: none, n: 0}
`)
	c = scheduled(t, e2, s2, 0)
	if err := e2.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	r, _ := s2.GetRun(ctx, c.ID)
	if r.Status != store.StatusSkipped || *r.Gate != "items_above" || fr.calls != 1 || !strings.Contains(*r.Summary, "none has 0 records") {
		t.Fatalf("run = %s %v %q, calls %d", r.Status, r.Gate, *r.Summary, fr.calls)
	}
}
