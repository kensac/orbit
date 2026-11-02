package store_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// finished runs one manual run of agent to status with cost, created at.
func finished(t *testing.T, s *store.Store, agent, status string, cost *float64, at string) string {
	t.Helper()
	id := manual(t, s, agent)
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil || c.ID != id {
		t.Fatalf("claim = %v %v, want %s", c, err, id)
	}
	if err := s.FinishRun(ctx, id, store.RunResult{Attempt: 1, Status: status, CostUSD: cost}); err != nil {
		t.Fatal(err)
	}
	createdAt(t, s, id, at)
	return id
}

func createdAt(t *testing.T, s *store.Store, id, at string) {
	t.Helper()
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET created_at = $2 WHERE id = $1`, id, at); err != nil {
		t.Fatal(err)
	}
}

func usd(v float64) *float64 { return &v }

func TestCostsAreExactAndBucketedByLocalDay(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a", "b")
	// 03:30Z on Oct 1 is Sep 30 in New York: the day is local, not UTC.
	finished(t, s, "a", "succeeded", usd(0.1), "2026-10-01T03:30:00Z")
	finished(t, s, "a", "failed", usd(0.2), "2026-10-01T12:00:00Z")
	finished(t, s, "a", "succeeded", nil, "2026-10-02T12:00:00Z")
	// Outside the range on both sides.
	finished(t, s, "b", "succeeded", usd(5), "2026-09-30T03:59:59Z")
	finished(t, s, "b", "succeeded", usd(7), "2026-10-03T04:00:00Z")
	createdAt(t, s, manual(t, s, "b"), "2026-10-02T12:00:00Z")

	rep, err := s.Costs(ctx, "2026-09-30", "2026-10-02", "America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	// 0.1 + 0.2 is 0.300000 exactly; a float sum would not be.
	if rep.Total.APIEquivalentUSD != "0.300000" || rep.Total.Runs != 4 || rep.Total.RunsWithCost != 2 {
		t.Fatalf("total = %+v", rep.Total)
	}
	want := map[string]int{"queued": 1, "running": 0, "succeeded": 2, "failed": 1, "cancelled": 0}
	for k, v := range want {
		if rep.Total.ByStatus[k] != v {
			t.Fatalf("total by status = %v, want %v", rep.Total.ByStatus, want)
		}
	}
	if len(rep.ByAgent) != 2 || rep.ByAgent[0].Agent != "a" || rep.ByAgent[0].APIEquivalentUSD != "0.300000" ||
		rep.ByAgent[0].Runs != 3 || rep.ByAgent[0].ByStatus["succeeded"] != 2 || rep.ByAgent[0].ByStatus["queued"] != 0 ||
		rep.ByAgent[1].Agent != "b" || rep.ByAgent[1].APIEquivalentUSD != "0.000000" || rep.ByAgent[1].ByStatus["queued"] != 1 {
		t.Fatalf("by agent = %+v", rep.ByAgent)
	}
	days := []store.DayCost{
		{Date: "2026-09-30", APIEquivalentUSD: "0.100000", Runs: 1},
		{Date: "2026-10-01", APIEquivalentUSD: "0.200000", Runs: 1},
		{Date: "2026-10-02", APIEquivalentUSD: "0.000000", Runs: 2},
	}
	if len(rep.ByDay) != len(days) {
		t.Fatalf("by day = %+v", rep.ByDay)
	}
	for i := range days {
		if rep.ByDay[i] != days[i] {
			t.Fatalf("by day = %+v, want %+v", rep.ByDay, days)
		}
	}

	// An empty range still lists its days.
	rep, err = s.Costs(ctx, "2025-01-01", "2025-01-02", "UTC")
	if err != nil || len(rep.ByAgent) != 0 || len(rep.ByDay) != 2 || rep.Total.Runs != 0 || rep.Total.APIEquivalentUSD != "0.000000" {
		t.Fatalf("empty range = %+v %v", rep, err)
	}
}

func TestRateLimitEventsAreRecentOnly(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	recent := finished(t, s, "a", "succeeded", nil, time.Now().Add(-time.Hour).Format(time.RFC3339))
	old := finished(t, s, "a", "succeeded", nil, time.Now().Add(-store.RateLimitLookback-time.Hour).Format(time.RFC3339))
	line := []byte(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`)
	for _, id := range []string{recent, old} {
		if err := s.AppendEvent(ctx, id, 1, 1, "rate_limit_event", line); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendEvent(ctx, id, 1, 2, "assistant", []byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	ev, err := s.RateLimitEvents(ctx)
	if err != nil || len(ev) != 1 || ev[0].RunID != recent || !json.Valid(ev[0].Payload) {
		t.Fatalf("events = %+v %v", ev, err)
	}
}

func TestHealth(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a", "b", "c")
	done := finished(t, s, "a", "succeeded", nil, time.Now().Format(time.RFC3339))
	if _, _, err := s.EnqueueContinuation(ctx, store.Continuation{Agent: "a", FromRun: done, After: time.Hour, MaxChain: 3}); err != nil {
		t.Fatal(err)
	}
	running := manual(t, s, "b")
	if c, err := s.ClaimRun(ctx, "host/0"); err != nil || c.ID != running {
		t.Fatalf("claim = %v %v", c, err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET heartbeat_at = now() - interval '90 seconds' WHERE id = $1`, running); err != nil {
		t.Fatal(err)
	}
	manual(t, s, "b") // due, waiting behind b's running run

	h, err := s.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.Queue.Due != 1 || h.Queue.WaitingContinuations != 1 || h.Queue.OldestDueAt == nil {
		t.Fatalf("queue = %+v", h.Queue)
	}
	if len(h.Running) != 1 || h.Running[0].RunID != running || *h.Running[0].Worker != "host/0" ||
		*h.Running[0].HeartbeatAgeSeconds < 89 || *h.Running[0].HeartbeatAgeSeconds > 120 {
		t.Fatalf("running = %+v", h.Running)
	}
	if ls := h.LastSuccess["a"]; ls == nil || ls.RunID != done || h.LastSuccess["b"] != nil || h.PendingActions != 0 {
		t.Fatalf("last success = %+v, pending %d", h.LastSuccess, h.PendingActions)
	}
}

// The startup seed of the last-success gauge counts a skipped run as a
// success and a failure as none.
func TestLastSuccessTimesCountSkipped(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a", "b", "c")
	finished(t, s, "a", store.StatusSucceeded, nil, "2026-01-01T00:00:00Z")
	skip := manual(t, s, "b")
	if c, err := s.ClaimRun(ctx, "w"); err != nil || c == nil || c.ID != skip {
		t.Fatalf("claim = %v %v", c, err)
	}
	if err := s.FinishRun(ctx, skip, store.RunResult{Attempt: 1, Status: store.StatusSkipped, Gate: "unchanged"}); err != nil {
		t.Fatal(err)
	}
	finished(t, s, "c", store.StatusFailed, nil, "2026-01-01T00:00:00Z")
	last, err := s.LastSuccessTimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := last["a"]; !ok || len(last) != 2 {
		t.Fatalf("last = %v, want a and b", last)
	}
	if _, ok := last["b"]; !ok {
		t.Fatalf("last = %v, want b's skipped run", last)
	}
}

// The gauges count pending actions and only the queued runs due now.
func TestGaugeCountsQueuedDueOnly(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	manual(t, s, "a")
	later := manual(t, s, "a")
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET run_after = now() + interval '1 hour' WHERE id = $1`, later); err != nil {
		t.Fatal(err)
	}
	pending, queued, err := s.GaugeCounts(ctx)
	if err != nil || pending != 0 || queued != 1 {
		t.Fatalf("gauges = %d pending, %d queued, %v; want 0, 1", pending, queued, err)
	}
}
