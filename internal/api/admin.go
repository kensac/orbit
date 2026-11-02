package api

import (
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/kensac/orbit/internal/scheduler"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/usage"
)

// defaultUsageDays is the range GET /usage covers without from and to:
// the last 30 days, today included.
const defaultUsageDays = 30

// usage serves GET /usage: what Claude Code last reported about the
// subscription's windows, and the API-equivalent cost of runs over a range
// of days in the deployment's timezone (docs/api.md).
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	cfg := s.Engine.Current().Config
	tz := cfg.Deployment.Timezone
	loc, err := time.LoadLocation(tz)
	if err != nil {
		respond(w, nil, fmt.Errorf("deployment timezone %q: %w", tz, err))
		return
	}
	now := time.Now()
	from, to, err := usageRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"), now.In(loc))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	reading, err := usage.Read(r.Context(), s.Store, now)
	if err != nil {
		respond(w, nil, err)
		return
	}
	held, err := s.Store.HeldRuns(r.Context())
	if err != nil {
		respond(w, nil, err)
		return
	}
	costs, err := s.Store.Costs(r.Context(), from, to, tz)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{
		"timezone": tz,
		"from":     from,
		"to":       to,
		"subscription": map[string]any{
			"windows": orEmpty(reading.Windows),
			"overage": reading.Overage,
			"poll":    s.pollStatus(reading),
		},
		"deferral": map[string]any{
			"enabled":   cfg.Deployment.DeferLowPriority,
			"threshold": usage.DeferLowAbove,
			"held":      orEmpty(held),
		},
		"cost": costs,
	}, nil)
}

// pollStatus is the usage poller's state as /usage reports it.
func (s *Server) pollStatus(r *usage.Reading) map[string]any {
	out := map[string]any{"enabled": false, "every": nil, "last_ok_at": nil, "last_at": nil, "last_error": nil}
	if p := s.Engine.Current().Config.Deployment.UsagePoll; p != nil {
		out["enabled"], out["every"] = true, time.Duration(p.Every).String()
	}
	if r.LastPollOK != nil {
		out["last_ok_at"] = r.LastPollOK.PolledAt
	}
	if l := r.LastPoll; l != nil {
		out["last_at"] = l.PolledAt
		if !l.OK {
			out["last_error"] = l.Error
		}
	}
	return out
}

// usageRange resolves from and to (YYYY-MM-DD, inclusive). Either may be
// omitted: to defaults to today, from to defaultUsageDays days ending at to.
func usageRange(fromQ, toQ string, today time.Time) (string, string, error) {
	const layout = time.DateOnly
	to := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	if toQ != "" {
		t, err := time.Parse(layout, toQ)
		if err != nil {
			return "", "", fmt.Errorf("to: want YYYY-MM-DD: %w", err)
		}
		to = t
	}
	from := to.AddDate(0, 0, -(defaultUsageDays - 1))
	if fromQ != "" {
		f, err := time.Parse(layout, fromQ)
		if err != nil {
			return "", "", fmt.Errorf("from: want YYYY-MM-DD: %w", err)
		}
		from = f
	}
	if from.After(to) {
		return "", "", fmt.Errorf("from %s is after to %s", from.Format(layout), to.Format(layout))
	}
	if days := int(to.Sub(from).Hours()/24) + 1; days > store.MaxUsageDays {
		return "", "", fmt.Errorf("range is %d days; at most %d", days, store.MaxUsageDays)
	}
	return from.Format(layout), to.Format(layout), nil
}

// healthAgent is one configured agent in GET /health.
type healthAgent struct {
	Name        string               `json:"name"`
	Enabled     bool                 `json:"enabled"`
	Priority    string               `json:"priority"`
	Timezone    string               `json:"timezone"`
	LastSuccess *store.LastSuccess   `json:"last_success"`
	NextFires   []scheduler.NextFire `json:"next_fires"`
}

// health serves GET /health: configuration, queue, running runs, pending
// actions, and per agent its last success and next fires (docs/api.md).
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	h, err := s.Store.Health(r.Context())
	if err != nil {
		respond(w, nil, err)
		return
	}
	var cfgStatus any
	if s.ConfigState != nil {
		cfgStatus = s.ConfigState.Status()
	}
	cfg := s.Engine.Current().Config
	now := time.Now()
	agents := make([]healthAgent, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		priority := spec.PriorityNormal
		if a.Low() {
			priority = spec.PriorityLow
		}
		agents = append(agents, healthAgent{Name: a.Name, Enabled: a.IsEnabled(), Priority: priority, Timezone: a.Timezone,
			LastSuccess: h.LastSuccess[a.Name], NextFires: scheduler.NextFires(a, now)})
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	respond(w, map[string]any{
		"checked_at":      h.CheckedAt,
		"config":          cfgStatus,
		"queue":           h.Queue,
		"running":         h.Running,
		"pending_actions": h.PendingActions,
		"agents":          agents,
	}, nil)
}
