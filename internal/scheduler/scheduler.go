// Package scheduler turns agents' schedule triggers into queued runs. It
// only enqueues; the executor runs. One process schedules at a time, held by
// a Postgres advisory lock, and runs_one_per_fire makes a fire enqueue at
// most once even if two ever did.
package scheduler

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// lockKey is orbit's advisory lock id: "orbit" in ASCII.
const lockKey = 0x6f72626974

// Scheduler enqueues schedule fires.
type Scheduler struct {
	Store *store.Store
	// Config returns the configuration in force; it may change between
	// calls (reload).
	Config func() *spec.Config
	Log    *slog.Logger
	// Reload wakes the loop when configuration changes.
	Reload <-chan struct{}
	Now    func() time.Time
}

// fire is one upcoming trigger time.
type fire struct {
	agent   *spec.Agent
	index   int
	at      time.Time
	trigger spec.Trigger
}

// Run blocks until ctx ends. It waits for the leader lock, catches up, then
// sleeps until each next fire.
func (s *Scheduler) Run(ctx context.Context) error {
	conn, err := s.acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	s.Log.Info("scheduler is leader")

	// Fires missed while no process was leader come from the database;
	// fires reached while leader come from the clock, as (prev, now].
	if err := s.catchUp(ctx, s.Config()); err != nil {
		return err
	}
	prev := s.Now()
	for {
		cfg := s.Config()
		now := s.Now()
		s.Tick(ctx, cfg, prev, now)
		prev = now

		// Wake at the next fire, and at least hourly so a clock step or a
		// suspended host is re-evaluated rather than trusted.
		d := time.Hour
		if next := s.upcoming(cfg, now); len(next) > 0 && time.Until(next[0].at) < d {
			d = time.Until(next[0].at) + time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.Reload:
		case <-time.After(d):
		}
		// Keep the lock's connection alive and detect losing it.
		if err := conn.Ping(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// acquire blocks until this process holds the scheduler lock. The lock lives
// on one pooled connection held for the scheduler's lifetime; if that
// connection drops, Postgres releases the lock and Run returns an error.
func (s *Scheduler) acquire(ctx context.Context) (*pgxpool.Conn, error) {
	for {
		conn, err := s.Store.Pool.Acquire(ctx)
		if err != nil {
			return nil, err
		}
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(lockKey)).Scan(&ok); err != nil {
			conn.Release()
			return nil, err
		}
		if ok {
			return conn, nil
		}
		conn.Release()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

// catchUp enqueues, for each trigger, the newest fire that is due and not
// yet recorded. Only the newest: a host that was down for a week runs a
// daily agent once, not seven times (systemd's Persistent=true does the
// same). A trigger that has never fired is not backfilled, so deploying a
// new agent does not run it on the spot.
func (s *Scheduler) catchUp(ctx context.Context, cfg *spec.Config) error {
	now := s.Now()
	for _, a := range sortedAgents(cfg) {
		if !a.IsEnabled() {
			continue
		}
		loc, _ := time.LoadLocation(a.Timezone)
		for i, t := range a.Triggers {
			sched, err := spec.ParseSchedule(t.Schedule)
			if err != nil {
				continue
			}
			last, err := s.Store.LastScheduledFire(ctx, a.Name, i)
			if err != nil {
				return err
			}
			if last == nil {
				continue
			}
			if due, ok := newestFire(sched, last.In(loc), now); ok {
				s.enqueue(ctx, fire{agent: a, index: i, at: due, trigger: t})
			}
		}
	}
	return nil
}

// upcoming lists each enabled trigger's next fire after now, soonest first.
func (s *Scheduler) upcoming(cfg *spec.Config, now time.Time) []fire {
	var out []fire
	for _, a := range sortedAgents(cfg) {
		if !a.IsEnabled() {
			continue
		}
		loc, _ := time.LoadLocation(a.Timezone)
		for i, t := range a.Triggers {
			sched, err := spec.ParseSchedule(t.Schedule)
			if err != nil {
				continue
			}
			out = append(out, fire{agent: a, index: i, at: sched.Next(now.In(loc)), trigger: t})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// Tick enqueues, per trigger, the newest fire in (since, now].
func (s *Scheduler) Tick(ctx context.Context, cfg *spec.Config, since, now time.Time) {
	for _, a := range sortedAgents(cfg) {
		if !a.IsEnabled() {
			continue
		}
		loc, _ := time.LoadLocation(a.Timezone)
		for i, t := range a.Triggers {
			sched, err := spec.ParseSchedule(t.Schedule)
			if err != nil {
				continue
			}
			if due, ok := newestFire(sched, since.In(loc), now); ok {
				s.enqueue(ctx, fire{agent: a, index: i, at: due, trigger: t})
			}
		}
	}
}

// newestFire is the latest fire of sched in (since, now].
func newestFire(sched cron.Schedule, since, now time.Time) (time.Time, bool) {
	var due time.Time
	found := false
	for f := sched.Next(since); !f.After(now); f = sched.Next(f) {
		due, found = f, true
	}
	return due, found
}

func (s *Scheduler) enqueue(ctx context.Context, f fire) {
	idx := f.index
	at := f.at.UTC()
	id, err := s.Store.EnqueueRun(ctx, store.NewRun{
		Agent: f.agent.Name, SpecSHA: f.agent.SHA, Trigger: "schedule",
		TriggerIndex: &idx, ScheduledFor: &at, Prompt: f.trigger.Prompt,
	})
	if err != nil {
		s.Log.Error("enqueue fire", "agent", f.agent.Name, "trigger", f.index, "at", at, "err", err)
		return
	}
	if id != "" {
		s.Log.Info("enqueued", "agent", f.agent.Name, "trigger", f.index, "at", at, "run", id)
	}
}

func sortedAgents(cfg *spec.Config) []*spec.Agent {
	out := make([]*spec.Agent, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
