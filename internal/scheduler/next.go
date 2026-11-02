package scheduler

import (
	"sort"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// NextFire is when one of an agent's schedule triggers fires next: a cron
// Schedule, or an RRule from its Start.
type NextFire struct {
	TriggerIndex int       `json:"trigger_index"`
	Schedule     string    `json:"schedule,omitempty"`
	RRule        string    `json:"rrule,omitempty"`
	Start        string    `json:"start,omitempty"`
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
	loc := a.Location()
	for i, t := range a.Triggers {
		sched, err := spec.TriggerSchedule(t, loc)
		if err != nil {
			continue
		}
		next := sched.Next(now.In(loc))
		if next.IsZero() {
			continue
		}
		out = append(out, NextFire{TriggerIndex: i, Schedule: t.Schedule, RRule: t.RRule, Start: t.Start, At: next.UTC()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}
