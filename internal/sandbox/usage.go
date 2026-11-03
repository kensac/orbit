package sandbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"
)

// An account's usage: the subscription's rate-limit windows, as Claude Code
// reported them to the box's status line (UsageStatusline) or as the
// undocumented /api/oauth/usage answered a poll (UsagePoll).
const (
	UsageStatusline = "statusline"
	UsagePoll       = "poll"
	// MaxUsageBytes bounds what is read of either.
	MaxUsageBytes = 64 << 10
	maxWindows    = 20
)

// UsageWindow is one rate-limit window.
type UsageWindow struct {
	// Window is the CLI's or the endpoint's name: five_hour, seven_day, ...
	Window string `json:"window"`
	// UsedPercentage is the share of the window used, 0 to 100; nil when
	// not reported.
	UsedPercentage *float64   `json:"used_percentage"`
	ResetsAt       *time.Time `json:"resets_at"`
}

// AccountUsage is an account's usage at ObservedAt; a zero ObservedAt is
// no observation.
type AccountUsage struct {
	Source     string        `json:"source"`
	ObservedAt *time.Time    `json:"observed_at"`
	Windows    []UsageWindow `json:"windows"`
}

var windowRE = regexp.MustCompile(`^[a-z0-9_]{1,40}$`)

// ParseStatuslineUsage reads the status line's record:
// {"observed_at": <unix seconds>, "rate_limits": {"five_hour": {"used_percentage": 12.5, "resets_at": <unix seconds>}, ...}}.
// Empty input is no observation.
func ParseStatuslineUsage(b []byte) (AccountUsage, error) {
	u := AccountUsage{Source: UsageStatusline}
	if len(bytes.TrimSpace(b)) == 0 {
		return u, nil
	}
	var rec struct {
		ObservedAt float64                    `json:"observed_at"`
		RateLimits map[string]json.RawMessage `json:"rate_limits"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return u, fmt.Errorf("usage record: %w", err)
	}
	at, ok := epoch(rec.ObservedAt)
	if !ok {
		return u, errors.New("usage record: no observed_at")
	}
	u.ObservedAt = &at
	ws, err := windows(rec.RateLimits, "used_percentage")
	u.Windows = ws
	return u, err
}

// ParseOAuthUsage reads /api/oauth/usage's answer, observed at now: each
// top-level object with a utilization (a percentage) and resets_at is a
// window; anything else (extra usage, nulls) is left out.
func ParseOAuthUsage(b []byte, now time.Time) (AccountUsage, error) {
	u := AccountUsage{Source: UsagePoll, ObservedAt: &now}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return u, fmt.Errorf("usage answer: %w", err)
	}
	ws, err := windows(top, "utilization")
	u.Windows = ws
	return u, err
}

func windows(m map[string]json.RawMessage, pctField string) ([]UsageWindow, error) {
	var out []UsageWindow
	for name, raw := range m {
		var w map[string]json.RawMessage
		if json.Unmarshal(raw, &w) != nil || w == nil {
			continue
		}
		pctRaw, hasPct := w[pctField]
		resetRaw, hasReset := w["resets_at"]
		if !hasPct && !hasReset {
			continue
		}
		if !windowRE.MatchString(name) {
			return nil, fmt.Errorf("window name %q", truncateString(name, 50))
		}
		uw := UsageWindow{Window: name}
		var pct *float64
		if hasPct && json.Unmarshal(pctRaw, &pct) == nil && pct != nil && !math.IsNaN(*pct) && *pct >= 0 && *pct <= 1000 {
			uw.UsedPercentage = pct
		}
		if hasReset {
			uw.ResetsAt = resetTime(resetRaw)
		}
		out = append(out, uw)
	}
	if len(out) > maxWindows {
		return nil, fmt.Errorf("%d windows, more than %d", len(out), maxWindows)
	}
	slices.SortFunc(out, func(a, b UsageWindow) int { return strings.Compare(a.Window, b.Window) })
	return out, nil
}

// resetTime is a resets_at: unix seconds (milliseconds when that large), or
// an RFC 3339 string; nil for anything else.
func resetTime(raw json.RawMessage) *time.Time {
	var n float64
	if json.Unmarshal(raw, &n) == nil {
		if t, ok := epoch(n); ok {
			return &t
		}
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

func epoch(n float64) (time.Time, bool) {
	if n <= 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return time.Time{}, false
	}
	if n > 1e12 {
		n /= 1000
	}
	return time.Unix(int64(n), 0).UTC(), true
}

func truncateString(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
