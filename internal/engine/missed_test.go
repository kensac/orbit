package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
)

// TestALateFireIsSkippedAsMissed: a schedule fire claimed past its trigger's grace ends skipped, gate missed, without a model call or a notification, and is no success for staleness; one within the grace runs.
func TestALateFireIsSkippedAsMissed(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, h := harnessFiles(t, fr, "", nil)
	a := e.Current().Config.Agents["finance"]
	fire := func(at time.Time) *store.Run {
		t.Helper()
		idx := 0
		if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "finance", SpecSHA: a.SHA, Trigger: "schedule", TriggerIndex: &idx, ScheduledFor: &at}); err != nil {
			t.Fatal(err)
		}
		c, err := s.ClaimRun(ctx, "test")
		if err != nil || c == nil {
			t.Fatalf("claim: %v %v", c, err)
		}
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		r, err := s.GetRun(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// A monthly trigger's default grace is the cap, an hour.
	r := fire(time.Now().Add(-2 * time.Hour).Truncate(time.Second))
	if r.Status != store.StatusSkipped || r.Gate == nil || *r.Gate != store.GateMissed || fr.calls != 0 {
		t.Fatalf("two hours late = %s gate %v, calls %d", r.Status, r.Gate, fr.calls)
	}
	if !strings.Contains(*r.Summary, "past its grace of 1h0m0s") {
		t.Fatalf("summary %q", *r.Summary)
	}
	h.mu.Lock()
	if len(h.messages) != 0 {
		t.Fatalf("a missed fire notified: %v", h.messages)
	}
	h.mu.Unlock()
	hl, err := s.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hl.LastSuccess["finance"] != nil {
		t.Fatalf("last success = %+v: a missed fire did no work", hl.LastSuccess["finance"])
	}

	r = fire(time.Now().Add(-10 * time.Minute).Truncate(time.Second))
	if r.Status != store.StatusSucceeded || fr.calls != 1 {
		t.Fatalf("ten minutes late = %s, calls %d: within the grace it runs", r.Status, fr.calls)
	}
}
