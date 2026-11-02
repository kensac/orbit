// Package usage reads the subscription's usage windows from the two places
// orbit learns them, and decides whether low-priority work waits.
//
// Claude Code reports a window in a run's stream (rate_limit_event, stored
// in run_events) when its rate-limit information changes; that is exact
// but only as fresh as the last run. The optional poller (poll.go) asks the
// endpoint the CLI's own /usage screen reads, so the reading stays fresh
// between runs. Every reading names its source and time, and the freshest
// one per window wins.
package usage

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

// Sources of a window reading.
const (
	SourceStream = "stream"
	SourcePoll   = "poll"
)

// Window is the freshest reading of one subscription window.
type Window struct {
	Window string `json:"window"`
	// Status is allowed, allowed_warning or rejected as a stream reported
	// it; the endpoint reports none, so nil for a polled reading.
	Status *string `json:"status"`
	// Utilization is the fraction of the window used, 0 to 1.
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
	// Expired is true once ResetsAt has passed: the window has reset since
	// the reading, so Utilization no longer describes it.
	Expired    bool      `json:"expired"`
	ObservedAt time.Time `json:"observed_at"`
	Source     string    `json:"source"`
	// RunID is the run whose stream carried a stream reading.
	RunID *string `json:"run_id"`
}

// Reading is what /usage and the scheduler see.
type Reading struct {
	Windows []Window
	Overage *runner.RateLimitOverage
	// LastPoll and LastPollOK are the newest poll of any outcome and the
	// newest successful one (within store.RateLimitLookback); nil without.
	LastPoll, LastPollOK *store.UsageSample
}

// Read gathers the newest stream reports and the newest successful poll
// and keeps, per window, the fresher of the two.
func Read(ctx context.Context, st *store.Store, now time.Time) (*Reading, error) {
	events, err := st.RateLimitEvents(ctx)
	if err != nil {
		return nil, err
	}
	obs := make([]runner.RateLimitObservation, len(events))
	for i, e := range events {
		obs[i] = runner.RateLimitObservation{RunID: e.RunID, At: e.At, Payload: e.Payload}
	}
	streamed, overage := runner.RateLimits(obs, now)
	ok, last, err := st.LatestUsageSamples(ctx)
	if err != nil {
		return nil, err
	}
	var polled []Window
	if ok != nil {
		polled = ParsePoll(ok.Payload, ok.PolledAt, now)
	}
	return &Reading{Windows: Merge(streamed, polled), Overage: overage, LastPoll: last, LastPollOK: ok}, nil
}

// Merge keeps the fresher reading of each window, sorted by name; a tie
// goes to the stream, which is the CLI's own report.
func Merge(streamed []runner.RateLimitWindow, polled []Window) []Window {
	best := map[string]Window{}
	for _, w := range streamed {
		status, run := w.Status, w.RunID
		win := Window{Window: w.Window, Utilization: w.Utilization, ResetsAt: w.ResetsAt, Expired: w.Expired,
			ObservedAt: w.ObservedAt, Source: SourceStream, RunID: &run}
		if status != "" {
			win.Status = &status
		}
		best[w.Window] = win
	}
	for _, w := range polled {
		if b, ok := best[w.Window]; !ok || w.ObservedAt.After(b.ObservedAt) {
			best[w.Window] = w
		}
	}
	out := make([]Window, 0, len(best))
	for _, w := range best {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Window < out[j].Window })
	return out
}

// ParsePoll reads the endpoint's body: an object whose members are windows
// by name, each null or {"utilization": <percent 0 to 100>, "resets_at":
// <RFC 3339 or null>, ...}, beside other members (extra_usage, limits) that
// are not windows. A member is a window when it is an object with a numeric
// utilization; anything else is skipped, so an added member changes
// nothing. The shape is the one Claude Code 2.1.x caches from this
// endpoint (cachedUsageUtilization in ~/.claude.json, read 2026-11-01).
func ParsePoll(payload json.RawMessage, at, now time.Time) []Window {
	var body map[string]json.RawMessage
	if json.Unmarshal(payload, &body) != nil {
		return nil
	}
	var out []Window
	for name, raw := range body {
		var w struct {
			Utilization *float64 `json:"utilization"`
			ResetsAt    *string  `json:"resets_at"`
		}
		if json.Unmarshal(raw, &w) != nil || w.Utilization == nil {
			continue
		}
		// The endpoint says percent; a stream says a fraction. Readings
		// are compared, so both are fractions here.
		frac := *w.Utilization / 100
		win := Window{Window: name, Utilization: &frac, ObservedAt: at, Source: SourcePoll}
		if w.ResetsAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *w.ResetsAt); err == nil {
				t = t.UTC()
				win.ResetsAt = &t
				win.Expired = !t.After(now)
			}
		}
		out = append(out, win)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Window < out[j].Window })
	return out
}

// DeferLowAbove is the utilization (a fraction) above which a low-priority
// agent's scheduled runs wait. The fleet shares one subscription, so its
// last fifth is kept for the agents a person relies on and for the person's
// own use of Claude, which a background agent should never crowd out.
const DeferLowAbove = 0.8

// DeferRecheck is how long a held run waits before it is judged again:
// twice the default poll interval, so each judgement sees a fresh poll, and
// short beside the five-hour window.
const DeferRecheck = 10 * time.Minute

// Over returns the fullest window above DeferLowAbove that has not reset
// since it was read, or nil.
func Over(windows []Window) *Window {
	var top *Window
	for i := range windows {
		w := &windows[i]
		if w.Expired || w.Utilization == nil || *w.Utilization <= DeferLowAbove {
			continue
		}
		if top == nil || *w.Utilization > *top.Utilization {
			top = w
		}
	}
	return top
}

// Deferral is the store record of a hold because of w.
func (w *Window) Deferral() store.Deferral {
	return store.Deferral{Window: w.Window, Utilization: *w.Utilization, Threshold: DeferLowAbove,
		Source: w.Source, ObservedAt: w.ObservedAt}
}
