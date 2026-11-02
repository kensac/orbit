package scheduler

import (
	"context"
	"time"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/usage"
)

// Deferring low-priority work (orbit.yaml defer_low_priority). While a
// subscription window is above usage.DeferLowAbove, a scheduled run of a
// priority: low agent is held in the queue (its run_after pushed forward
// by usage.DeferRecheck) and the hold recorded (run_deferrals); once every
// window is below, held runs are let go at once. Only scheduled runs wait:
// a manual run or a person's message is someone asking now.

// DeferCheckEvery is how often the leader judges held and due low-priority
// runs, so a run waits at most this long past the window dropping.
const DeferCheckEvery = time.Minute

// lowAgents are the enabled priority: low agents, by name.
func lowAgents(cfg *spec.Config) []string {
	var out []string
	for _, a := range sortedAgents(cfg) {
		if a.Low() && a.IsEnabled() {
			out = append(out, a.Name)
		}
	}
	return out
}

// over is the window that holds low-priority work now, or nil: none is
// above the threshold, deferring is off, or the reading failed (best
// effort: a failed read holds nothing).
func (s *Scheduler) over(ctx context.Context, cfg *spec.Config) *usage.Window {
	if !cfg.Deployment.DeferLowPriority {
		return nil
	}
	r, err := usage.Read(ctx, s.Store, s.now())
	if err != nil {
		s.Log.Warn("defer: reading usage", "err", err)
		return nil
	}
	return usage.Over(r.Windows)
}

// DeferPass holds every due scheduled run of a low agent while a window is
// over, and lets go of every hold that no longer applies.
func (s *Scheduler) DeferPass(ctx context.Context, cfg *spec.Config) {
	w := s.over(ctx, cfg)
	var keep []string
	if w != nil {
		keep = lowAgents(cfg)
	}
	if n, err := s.Store.ReleaseDeferrals(ctx, keep); err != nil {
		s.Log.Warn("defer: release", "err", err)
	} else if n > 0 {
		s.Log.Info("defer: released held runs", "runs", n)
	}
	if w == nil || len(keep) == 0 {
		return
	}
	due, err := s.Store.DueScheduledRuns(ctx, keep)
	if err != nil {
		s.Log.Warn("defer: due runs", "err", err)
		return
	}
	until := s.now().Add(usage.DeferRecheck)
	for _, id := range due {
		if _, err := s.Store.DeferRun(ctx, id, until, w.Deferral()); err != nil {
			s.Log.Warn("defer: hold", "run", id, "err", err)
		}
	}
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
