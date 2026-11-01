package runner

import (
	"encoding/json"
	"testing"
	"time"
)

// cliLine is a rate_limit_event exactly as Claude Code 2.1.316 wrote it.
const cliLine = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1794146400,"rateLimitType":"seven_day","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"seven_day":{"utilization":0.14,"resetsAt":1794146400,"status":"allowed"}}},"uuid":"fe275fca-6b0b-4656-bc77-31f6a365e5fe","session_id":"3901bad4-3409-438f-9947-f65be9fc74d5"}`

func TestRateLimitsNewestPerWindow(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	obs := []RateLimitObservation{
		{RunID: "old", At: t0.Add(-2 * time.Hour), Payload: json.RawMessage(
			`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","rateLimitType":"five_hour","utilization":0.9,"resetsAt":1793530800}}`)},
		{RunID: "new", At: t0, Payload: json.RawMessage(cliLine)},
		{RunID: "older", At: t0.Add(-3 * time.Hour), Payload: json.RawMessage(
			`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"seven_day","utilization":0.5,"overageStatus":"allowed"}}`)},
		{RunID: "junk", At: t0.Add(time.Hour), Payload: json.RawMessage(`"not an object"`)},
	}
	windows, overage := RateLimits(obs, t0)
	if len(windows) != 2 {
		t.Fatalf("windows = %+v", windows)
	}
	five, seven := windows[0], windows[1]
	if five.Window != "five_hour" || five.RunID != "old" || five.Status != "allowed_warning" || *five.Utilization != 0.9 ||
		!five.ResetsAt.Equal(time.Unix(1793530800, 0)) || !five.Expired {
		t.Fatalf("five_hour = %+v", five)
	}
	if seven.Window != "seven_day" || seven.RunID != "new" || *seven.Utilization != 0.14 ||
		!seven.ResetsAt.Equal(time.Unix(1794146400, 0)) || seven.Expired || !seven.ObservedAt.Equal(t0) {
		t.Fatalf("seven_day = %+v", seven)
	}
	if overage == nil || overage.RunID != "new" || *overage.Status != "rejected" || *overage.DisabledReason != "org_level_disabled" ||
		*overage.InUse || overage.ResetsAt != nil {
		t.Fatalf("overage = %+v", overage)
	}

	if w, o := RateLimits(nil, t0); len(w) != 0 || o != nil {
		t.Fatalf("none = %v %v", w, o)
	}
	// A top-level window without utilization reports null, not zero.
	w, _ := RateLimits([]RateLimitObservation{{RunID: "r", At: t0, Payload: json.RawMessage(
		`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour"}}`)}}, t0)
	if len(w) != 1 || w[0].Utilization != nil || w[0].ResetsAt != nil || w[0].Status != "rejected" {
		t.Fatalf("bare = %+v", w)
	}
}
