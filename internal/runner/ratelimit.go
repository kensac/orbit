package runner

import (
	"encoding/json"
	"sort"
	"time"
)

// RateLimitObservation is one stored Claude Code rate_limit_event: the run
// whose stream carried it, when orbit stored it, and the line itself.
type RateLimitObservation struct {
	RunID   string
	At      time.Time
	Payload json.RawMessage
}

// RateLimitWindow is what Claude Code last reported about one subscription
// window (five_hour, seven_day, ...).
type RateLimitWindow struct {
	Window string `json:"window"`
	// Status is allowed, allowed_warning or rejected, as the CLI said it.
	Status string `json:"status"`
	// Utilization is the fraction of the window used, 0 to 1, when the CLI
	// reported one.
	Utilization *float64   `json:"utilization"`
	ResetsAt    *time.Time `json:"resets_at"`
	// Expired is true when ResetsAt has passed: the window has reset since
	// it was observed, so Utilization no longer describes it.
	Expired    bool      `json:"expired"`
	ObservedAt time.Time `json:"observed_at"`
	RunID      string    `json:"run_id"`
}

// RateLimitOverage is the overage state of the newest rate_limit_event.
type RateLimitOverage struct {
	Status         *string    `json:"status"`
	ResetsAt       *time.Time `json:"resets_at"`
	DisabledReason *string    `json:"disabled_reason"`
	InUse          *bool      `json:"in_use"`
	ObservedAt     time.Time  `json:"observed_at"`
	RunID          string     `json:"run_id"`
}

// rateLimitEvent is the line's shape as Claude Code 2.1.316 writes it
// (sdk.d.ts SDKRateLimitEvent, and seen from the real CLI):
//
//	{"type":"rate_limit_event","rate_limit_info":{"status":"allowed",
//	 "resetsAt":1794146400,"rateLimitType":"seven_day","overageStatus":"rejected",
//	 "overageDisabledReason":"org_level_disabled","isUsingOverage":false,
//	 "unifiedWindows":{"seven_day":{"utilization":0.14,"resetsAt":1794146400,
//	 "status":"allowed"}}},"uuid":"...","session_id":"..."}
//
// resetsAt is Unix seconds. unifiedWindows is not in the SDK's types; the
// top-level fields describe the one window named by rateLimitType.
type rateLimitEvent struct {
	Info struct {
		windowInfo
		RateLimitType         string                `json:"rateLimitType"`
		OverageStatus         *string               `json:"overageStatus"`
		OverageResetsAt       *float64              `json:"overageResetsAt"`
		OverageDisabledReason *string               `json:"overageDisabledReason"`
		IsUsingOverage        *bool                 `json:"isUsingOverage"`
		UnifiedWindows        map[string]windowInfo `json:"unifiedWindows"`
	} `json:"rate_limit_info"`
}

type windowInfo struct {
	Status      string   `json:"status"`
	ResetsAt    *float64 `json:"resetsAt"`
	Utilization *float64 `json:"utilization"`
}

// RateLimits reduces observations to the newest report per window, and the
// overage state of the newest event that parses. A line that does not parse
// is skipped. Windows are sorted by name; now decides Expired.
func RateLimits(obs []RateLimitObservation, now time.Time) ([]RateLimitWindow, *RateLimitOverage) {
	sorted := append([]RateLimitObservation(nil), obs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].At.After(sorted[j].At) })

	seen := map[string]RateLimitWindow{}
	var overage *RateLimitOverage
	for _, o := range sorted {
		var ev rateLimitEvent
		if json.Unmarshal(o.Payload, &ev) != nil {
			continue
		}
		info := ev.Info
		if overage == nil && (info.OverageStatus != nil || info.OverageDisabledReason != nil || info.IsUsingOverage != nil) {
			overage = &RateLimitOverage{Status: info.OverageStatus, ResetsAt: unixTime(info.OverageResetsAt),
				DisabledReason: info.OverageDisabledReason, InUse: info.IsUsingOverage, ObservedAt: o.At, RunID: o.RunID}
		}
		// Within one event, a unifiedWindows entry is the fuller report of
		// its window, so it wins over the top-level fields.
		here := map[string]windowInfo{}
		if info.RateLimitType != "" {
			here[info.RateLimitType] = info.windowInfo
		}
		for name, w := range info.UnifiedWindows {
			if name != "" {
				here[name] = w
			}
		}
		for name, w := range here {
			if _, ok := seen[name]; ok {
				continue
			}
			rw := RateLimitWindow{Window: name, Status: w.Status, Utilization: w.Utilization,
				ResetsAt: unixTime(w.ResetsAt), ObservedAt: o.At, RunID: o.RunID}
			rw.Expired = rw.ResetsAt != nil && !rw.ResetsAt.After(now)
			seen[name] = rw
		}
	}
	out := make([]RateLimitWindow, 0, len(seen))
	for _, w := range seen {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Window < out[j].Window })
	return out, overage
}

func unixTime(sec *float64) *time.Time {
	if sec == nil || *sec <= 0 {
		return nil
	}
	t := time.Unix(int64(*sec), 0).UTC()
	return &t
}
