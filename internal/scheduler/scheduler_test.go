package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

var ctx = context.Background()

func setup(t *testing.T, schedule string) (*Scheduler, *store.Store, *spec.Config) {
	t.Helper()
	s := testdb.New(t)
	a := &spec.Agent{Name: "a", Charter: "x", Timezone: "America/New_York", Triggers: []spec.Trigger{{Schedule: schedule, Prompt: "wake"}}, SHA: "sha-a"}
	cfg := &spec.Config{Agents: map[string]*spec.Agent{"a": a}}
	b, _ := json.Marshal(a)
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"a": {SHA: "sha-a", JSON: b, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	return &Scheduler{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Config: func() *spec.Config { return cfg }}, s, cfg
}

func count(t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE trigger = 'schedule'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTickEnqueuesTheNewestFireInTheWindow(t *testing.T) {
	sch, s, cfg := setup(t, "0 9 * * *")
	ny, _ := time.LoadLocation("America/New_York")
	since := time.Date(2026, 11, 1, 8, 0, 0, 0, ny)
	sch.Tick(ctx, cfg, since, since.Add(time.Hour))
	if count(t, s) != 1 {
		t.Fatal("the 09:00 fire should be enqueued")
	}
	// Ticking the same window again enqueues nothing new.
	sch.Tick(ctx, cfg, since, since.Add(time.Hour))
	if count(t, s) != 1 {
		t.Fatal("a fire was enqueued twice")
	}
	run, _ := s.ListRuns(ctx, store.RunFilter{Agent: "a"})
	want := time.Date(2026, 11, 1, 9, 0, 0, 0, ny)
	if !run[0].ScheduledFor.Equal(want) || run[0].Prompt != "wake" {
		t.Fatalf("run = %v %q, want %v", run[0].ScheduledFor, run[0].Prompt, want)
	}
}

func TestCatchUpRunsOnceAfterDowntimeAndNeverBackfillsANewTrigger(t *testing.T) {
	sch, s, cfg := setup(t, "0 9 * * *")
	ny, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, ny)
	sch.Now = func() time.Time { return now }

	if err := sch.catchUp(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if count(t, s) != 0 {
		t.Fatal("a trigger that never fired must not be backfilled")
	}

	// Last fired a week ago: one catch-up run, for the newest due fire.
	sch.Tick(ctx, cfg, time.Date(2026, 11, 1, 8, 0, 0, 0, ny), time.Date(2026, 11, 1, 10, 0, 0, 0, ny))
	if err := sch.catchUp(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if count(t, s) != 2 {
		t.Fatalf("runs = %d, want the original plus one catch-up", count(t, s))
	}
	last, _ := s.LastScheduledFire(ctx, "a", 0)
	if want := time.Date(2026, 11, 8, 9, 0, 0, 0, ny); !last.Equal(want) {
		t.Fatalf("caught up to %v, want %v", last, want)
	}
}

func TestDisabledAgentsDoNotFire(t *testing.T) {
	sch, s, cfg := setup(t, "* * * * *")
	off := false
	cfg.Agents["a"].Enabled = &off
	now := time.Now()
	sch.Tick(ctx, cfg, now.Add(-time.Hour), now)
	if count(t, s) != 0 {
		t.Fatal("a disabled agent fired")
	}
}
