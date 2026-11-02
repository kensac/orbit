package scheduler

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/usage"
)

func sample(t *testing.T, s *store.Store, fiveHourPercent float64) {
	t.Helper()
	resets := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339Nano)
	body := fmt.Sprintf(`{"five_hour":{"utilization":%v,"resets_at":%q},"seven_day":{"utilization":20,"resets_at":%q},"limits":[]}`,
		fiveHourPercent, resets, resets)
	if err := s.RecordUsageSample(ctx, "test", 200, json.RawMessage(body), nil); err != nil {
		t.Fatal(err)
	}
}

func TestLowPriorityScheduledRunsWaitWhileAWindowIsNearlySpent(t *testing.T) {
	sch, s, cfg := setup(t, "* * * * *")
	cfg.Deployment.DeferLowPriority = true
	cfg.Agents["a"].Priority = spec.PriorityLow
	b := &spec.Agent{Name: "b", Charter: "x", Timezone: "UTC", Triggers: []spec.Trigger{{Schedule: "* * * * *"}}, SHA: "sha-b"}
	cfg.Agents["b"] = b
	j, _ := json.Marshal(b)
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"a": {SHA: "sha-a", JSON: []byte(`{}`), Enabled: true},
		"b": {SHA: "sha-b", JSON: j, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	sample(t, s, 91)

	now := time.Now()
	sch.Tick(ctx, cfg, now.Add(-time.Minute), now)
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil || c.Agent != "b" {
		t.Fatalf("claim = %+v %v, want b's run: only the low agent waits", c, err)
	}
	if c, err := s.ClaimRun(ctx, "w"); err != nil || c != nil {
		t.Fatalf("claim = %+v %v, want the low agent's run held", c, err)
	}
	held, err := s.HeldRuns(ctx)
	if err != nil || len(held) != 1 || held[0].Agent != "a" || held[0].Window != "five_hour" || held[0].Utilization != 0.91 ||
		held[0].Source != usage.SourcePoll || held[0].Threshold != usage.DeferLowAbove || held[0].Times != 1 {
		t.Fatalf("held = %+v %v", held, err)
	}

	// A person's manual run of the low agent is never held.
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: "sha-a", Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	sch.DeferPass(ctx, cfg)
	m, err := s.ClaimRun(ctx, "w")
	if err != nil || m == nil || m.Trigger != "manual" {
		t.Fatalf("claim = %+v %v, want the manual run", m, err)
	}
	if err := s.FinishRun(ctx, m.ID, store.RunResult{Attempt: m.Attempt, Status: store.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}

	// Still over at the recheck: held again, counted.
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET run_after = now() - interval '1 second' WHERE id = $1`, held[0].RunID); err != nil {
		t.Fatal(err)
	}
	sch.DeferPass(ctx, cfg)
	if again, _ := s.HeldRuns(ctx); len(again) != 1 || again[0].Times != 2 {
		t.Fatalf("held after a recheck = %+v, want the same run held twice", again)
	}

	// The window drops: the run is let go at once.
	sample(t, s, 40)
	sch.DeferPass(ctx, cfg)
	if left, _ := s.HeldRuns(ctx); len(left) != 0 {
		t.Fatalf("still held: %+v", left)
	}
	if c, err := s.ClaimRun(ctx, "w"); err != nil || c == nil || c.ID != held[0].RunID {
		t.Fatalf("claim = %+v %v, want the released run", c, err)
	}
	var released *time.Time
	if err := s.Pool.QueryRow(ctx, `SELECT released_at FROM run_deferrals WHERE run_id = $1`, held[0].RunID).Scan(&released); err != nil || released == nil {
		t.Fatalf("released_at = %v %v, want the release recorded", released, err)
	}
}

func TestDeferringOffHoldsNothing(t *testing.T) {
	sch, s, cfg := setup(t, "* * * * *")
	cfg.Agents["a"].Priority = spec.PriorityLow
	sample(t, s, 99)
	now := time.Now()
	sch.Tick(ctx, cfg, now.Add(-time.Minute), now)
	if c, err := s.ClaimRun(ctx, "w"); err != nil || c == nil {
		t.Fatalf("claim = %+v %v, want the run: defer_low_priority is off", c, err)
	}
}
