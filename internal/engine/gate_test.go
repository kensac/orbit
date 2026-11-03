package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/store"
)

// fireBase is now, so a fire the tests enqueue is within its grace (missed.go).
var fireBase = time.Now().UTC().Truncate(time.Minute)

// scheduled enqueues and claims a schedule fire at the given minute.
func scheduled(t *testing.T, e *engine.Engine, s *store.Store, minute int) *store.ClaimedRun {
	t.Helper()
	a := e.Current().Config.Agents["finance"]
	idx, at := 0, fireBase.Add(time.Duration(minute)*time.Minute)
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

func TestBudgetSkipsScheduledRunsAndAlertsOncePerPeriod(t *testing.T) {
	fr := &fakeRunner{output: `{"summary":"within budget","report":"done","notify":{"send":false,"priority":"default","title":""},"memory":{"update":false,"content":""},"questions":[],"actions":[]}`}
	e, s, _ := harnessWith(t, fr, "budget: {period: month, max_cost_usd: 0.20, max_runs: 1}\n")
	first := scheduled(t, e, s, 0)
	if err := e.Execute(ctx, first); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRun(ctx, first.ID); r.Status != store.StatusSucceeded || fr.calls != 1 {
		t.Fatalf("first run = %s, calls %d", r.Status, fr.calls)
	}
	for minute := 1; minute <= 2; minute++ {
		c := scheduled(t, e, s, minute)
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		r, err := s.GetRun(ctx, c.ID)
		if err != nil || r.Status != store.StatusSkipped || r.Gate == nil || *r.Gate != store.GateBudget {
			t.Fatalf("budget run = %+v, %v", r, err)
		}
		if strings.Contains(*r.Report, "orbit did not call the model") == false {
			t.Fatalf("budget report = %q", *r.Report)
		}
	}
	if fr.calls != 1 {
		t.Fatalf("scheduled runs called backend %d times, want once", fr.calls)
	}
	items, err := s.ListAttention(ctx, 100)
	if err != nil || len(items) != 1 || items[0].Kind != store.AttentionBudget || !items[0].Push || !strings.Contains(items[0].Detail, "runs") {
		t.Fatalf("budget attention = %+v, %v", items, err)
	}
	manual := claim(t, e, s)
	if err := e.Execute(ctx, manual); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRun(ctx, manual.ID); r.Status != store.StatusSucceeded || fr.calls != 2 {
		t.Fatalf("manual run = %s, calls %d: a budget must not block a person's request", r.Status, fr.calls)
	}
	last, err := s.LastSuccessTimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if last["finance"].Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("manual success missing from staleness timestamp: %v", last)
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

// TestAPrecheckDecidesWhetherTheModelIsCalled: exit 1 skips (naming what
// it printed), 0 runs, any other status and a timeout fail the run, and
// the command sees the inputs and none of orbit's environment.
func TestAPrecheckDecidesWhetherTheModelIsCalled(t *testing.T) {
	t.Setenv("ORBIT_PROBE_SECRET", "s3cret")
	for _, c := range []struct {
		name, gate string
		status     string
		calls      int
		want       string
	}{
		{"nothing to do", `{kind: precheck, command: 'test -z "$ORBIT_PROBE_SECRET" && test -f "$ORBIT_INPUTS/tx.json" && test -f inputs/tx.json && echo no new rows; exit 1'}`,
			store.StatusSkipped, 0, "Skipped: precheck exited 1: no new rows"},
		{"work to do", `{kind: precheck, command: 'exit 0'}`, store.StatusSucceeded, 1, ""},
		{"broken", `{kind: precheck, command: 'echo boom >&2; exit 3'}`, "failed", 0, "precheck exited 3: boom"},
		{"too slow", `{kind: precheck, command: 'sleep 30', timeout: 1s}`, "failed", 0, "precheck timed out after 1s"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fr := &fakeRunner{output: goodOutput}
			e, s, _ := harnessWith(t, fr, "gates: ["+c.gate+"]\n")
			run := scheduled(t, e, s, 0)
			start := time.Now()
			if err := e.Execute(ctx, run); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Fatalf("took %s", d)
			}
			r, _ := s.GetRun(ctx, run.ID)
			if r.Status != c.status || fr.calls != c.calls {
				t.Fatalf("run = %s, calls %d (error %v)", r.Status, fr.calls, r.Error)
			}
			got := ""
			switch {
			case r.Status == store.StatusSkipped:
				got = *r.Summary
				if *r.Gate != "precheck" {
					t.Fatalf("gate = %s", *r.Gate)
				}
			case r.Error != nil:
				got = *r.Error
			}
			if !strings.Contains(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			evs, _ := s.ListEvents(ctx, run.ID, 0, 0, 100)
			found := false
			for _, ev := range evs {
				found = found || ev.Kind == "orbit.precheck"
			}
			if !found {
				t.Fatal("no orbit.precheck event")
			}
		})
	}
}
