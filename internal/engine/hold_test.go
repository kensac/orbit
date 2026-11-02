package engine_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

func enqueueFire(t *testing.T, e *engine.Engine, s *store.Store, minute int) string {
	t.Helper()
	a := e.Current().Config.Agents["finance"]
	idx, at := 0, fireBase.Add(time.Duration(minute)*time.Minute)
	id, err := s.EnqueueRun(ctx, store.NewRun{Agent: "finance", SpecSHA: a.SHA, Trigger: "schedule", TriggerIndex: &idx, ScheduledFor: &at})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func runClaimed(t *testing.T, e *engine.Engine, s *store.Store) *store.Run {
	t.Helper()
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetRun(ctx, c.ID)
	return r
}

func titles(h *hooks) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, m := range h.messages {
		out = append(out, m["title"].(string))
	}
	return out
}

// TestAnAuthFailureHoldsTheRunner: a backend that says it is not logged in fails that run and holds its runner with one notification; the next scheduled run waits queued instead of failing; a person's run is not held, and its success resumes the schedule.
func TestAnAuthFailureHoldsTheRunner(t *testing.T) {
	fr := &fakeRunner{output: goodOutput, err: fmt.Errorf("%w: claude-code success/: Not logged in · Please run /login", runner.ErrBackend)}
	e, s, h := harnessFiles(t, fr, "notify: {when: never}\n", nil)

	enqueueFire(t, e, s, 0)
	r := runClaimed(t, e, s)
	if r.Status != "failed" || !strings.Contains(*r.Error, "Not logged in") {
		t.Fatalf("first = %s %v", r.Status, r.Error)
	}
	holds, err := s.OpenHolds(ctx)
	if err != nil || len(holds) != 1 || holds[0].Runner != "fake" || holds[0].Kind != store.HoldAuth || holds[0].RunID == nil || *holds[0].RunID != r.ID {
		t.Fatalf("holds = %+v %v", holds, err)
	}
	if got := titles(h); len(got) != 1 || got[0] != "orbit: runner fake is held" {
		t.Fatalf("notifications = %q: one, for the hold, and no run failure", got)
	}

	held := enqueueFire(t, e, s, 1)
	if c, err := s.ClaimRun(ctx, "test"); err != nil || c != nil {
		t.Fatalf("claimed %+v %v: a held runner's scheduled run must wait", c, err)
	}
	// A person's manual run is not held; failing again, it reports its own failure, as any run a person started does.
	a := e.Current().Config.Agents["finance"]
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "finance", SpecSHA: a.SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	if r := runClaimed(t, e, s); r.Trigger != "manual" || r.Status != "failed" {
		t.Fatalf("manual while held = %s %s", r.Trigger, r.Status)
	}
	if got := titles(h); len(got) != 2 || got[1] != "finance run failed" {
		t.Fatalf("notifications = %q: the hold's, then the manual run's own failure", got)
	}
	// The person logs in; their next run succeeds and resumes the runner.
	fr.err = nil
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "finance", SpecSHA: a.SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	if r := runClaimed(t, e, s); r.Trigger != "manual" || r.Status != "succeeded" {
		t.Fatalf("manual after login = %s %s", r.Trigger, r.Status)
	}
	if holds, _ := s.OpenHolds(ctx); len(holds) != 0 {
		t.Fatalf("holds after a person's success = %+v", holds)
	}
	if r := runClaimed(t, e, s); r.ID != held || r.Status != "succeeded" {
		t.Fatalf("the held fire = %s %s, want %s succeeded", r.ID, r.Status, held)
	}
}

// TestARejectedWindowHoldsUntilItResetsAndAProbeResumes: a rejected rate_limit_event before the failure holds the runner until the window resets; a probe that succeeds closes the hold.
func TestARejectedWindowHoldsUntilItResetsAndAProbeResumes(t *testing.T) {
	resets := time.Now().Add(3 * time.Hour).Truncate(time.Second)
	fr := &fakeRunner{output: `{"ok": true}`, err: fmt.Errorf("%w: claude-code success/: Claude AI usage limit reached", runner.ErrBackend),
		events: []runner.Event{{Kind: "rate_limit_event", Payload: fmt.Appendf(nil,
			`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":%d}}`, resets.Unix())}}}
	e, s, _ := harnessFiles(t, fr, "notify: {when: never}\n", nil)
	enqueueFire(t, e, s, 0)
	if r := runClaimed(t, e, s); r.Status != "failed" {
		t.Fatalf("run = %s", r.Status)
	}
	holds, _ := s.OpenHolds(ctx)
	if len(holds) != 1 || holds[0].Kind != store.HoldRateLimit || holds[0].ResetsAt == nil || !holds[0].ResetsAt.Equal(resets) ||
		!holds[0].NextProbeAt.Equal(resets) || !strings.Contains(holds[0].Reason, "five_hour") {
		t.Fatalf("holds = %+v", holds)
	}
	// Not due before the reset.
	if n, err := e.ProbeHolds(ctx); err != nil || n != 0 {
		t.Fatalf("probes before the reset = %d %v", n, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE backend_holds SET next_probe_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	fr.err, fr.events = nil, nil
	if n, err := e.ProbeHolds(ctx); err != nil || n != 1 {
		t.Fatalf("probes = %d %v", n, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		holds, _ = s.OpenHolds(ctx)
		if len(holds) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the probe did not close the hold: %+v", holds)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var by string
	if err := s.Pool.QueryRow(ctx, `SELECT closed_by FROM backend_holds`).Scan(&by); err != nil || by != "probe" {
		t.Fatalf("closed_by = %q %v", by, err)
	}
}
