package sandbox

import (
	"testing"
	"time"
)

func TestParseStatuslineUsage(t *testing.T) {
	u, err := ParseStatuslineUsage(nil)
	if err != nil || u.ObservedAt != nil || u.Source != UsageStatusline {
		t.Fatalf("no record: %+v %v", u, err)
	}
	u, err = ParseStatuslineUsage([]byte(`{"observed_at": 1793000000, "rate_limits": {
		"seven_day": {"used_percentage": 40, "resets_at": 1793500000},
		"five_hour": {"used_percentage": 12.5, "resets_at": 1793000500000},
		"spend_limit": {"used_percentage": 3, "resets_at": "2026-11-09T00:00:00Z", "used_usd": 1.5},
		"later": null, "flag": true}}`))
	if err != nil || !u.ObservedAt.Equal(time.Unix(1793000000, 0)) || len(u.Windows) != 3 {
		t.Fatalf("record: %+v %v", u, err)
	}
	w := u.Windows[0]
	if w.Window != "five_hour" || *w.UsedPercentage != 12.5 || !w.ResetsAt.Equal(time.Unix(1793000500, 0)) {
		t.Fatalf("milliseconds read as seconds: %+v", w)
	}
	if u.Windows[1].Window != "seven_day" || u.Windows[2].Window != "spend_limit" || !u.Windows[2].ResetsAt.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("windows %+v", u.Windows)
	}
	for _, bad := range []string{`{"rate_limits": {}}`, `[]`, `{"observed_at": 1, "rate_limits": {"Bad Name": {"used_percentage": 1}}}`} {
		if _, err := ParseStatuslineUsage([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestParseOAuthUsage(t *testing.T) {
	now := time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)
	u, err := ParseOAuthUsage([]byte(`{"five_hour": {"utilization": 6.0, "resets_at": "2026-11-02T09:59:59.943648+00:00"},
		"seven_day": {"utilization": 35, "resets_at": null}, "seven_day_opus": null, "extra_usage": {"is_enabled": false}}`), now)
	if err != nil || u.Source != UsagePoll || !u.ObservedAt.Equal(now) || len(u.Windows) != 2 {
		t.Fatalf("%+v %v", u, err)
	}
	if w := u.Windows[0]; w.Window != "five_hour" || *w.UsedPercentage != 6 || w.ResetsAt.Hour() != 9 {
		t.Fatalf("five_hour %+v", w)
	}
	if w := u.Windows[1]; *w.UsedPercentage != 35 || w.ResetsAt != nil {
		t.Fatalf("seven_day %+v", w)
	}
	if _, err := ParseOAuthUsage([]byte(`<html>`), now); err == nil {
		t.Fatal("an answer that is not JSON")
	}
}
