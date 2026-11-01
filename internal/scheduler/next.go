package scheduler

import (
	"sort"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// NextFire is when one of an agent's schedule triggers fires next.
type NextFire struct {
	TriggerIndex int       `json:"trigger_index"`
	Schedule     string    `json:"schedule"`
	At           time.Time `json:"at"`
}

// NextFires is each trigger's next fire after now, read in the agent's
// timezone as the scheduler reads it, soonest first. A disabled agent has
// none. Times are UTC.
func NextFires(a *spec.Agent, now time.Time) []NextFire {
	out := []NextFire{}
	if a == nil || !a.IsEnabled() {
		return out
	}
	loc, err := time.LoadLocation(a.Timezone)
	if err != nil {
		return out
	}
	for i, t := range a.Triggers {
		sched, err := spec.ParseSchedule(t.Schedule)
		if err != nil {
			continue
		}
		next := sched.Next(now.In(loc))
		if next.IsZero() {
			continue
		}
		out = append(out, NextFire{TriggerIndex: i, Schedule: t.Schedule, At: next.UTC()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}
