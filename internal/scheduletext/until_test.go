package scheduletext

import (
	"testing"
	"time"
)

func TestParseUntil(t *testing.T) {
	loc := location(t, "America/New_York")
	now := time.Date(2026, 11, 4, 15, 30, 0, 0, loc) // a Wednesday
	// A date alone, or a weekday, is that day's local midnight.
	for text, want := range map[string]string{
		"monday":               "2026-11-09T00:00:00-05:00",
		"until Monday":         "2026-11-09T00:00:00-05:00",
		"next wednesday":       "2026-11-11T00:00:00-05:00",
		"wednesday":            "2026-11-11T00:00:00-05:00",
		"tomorrow":             "2026-11-05T00:00:00-05:00",
		"tomorrow at 9am":      "2026-11-05T09:00:00-05:00",
		"today at 18:00":       "2026-11-04T18:00:00-05:00",
		"friday at 8":          "2026-11-06T08:00:00-05:00",
		"2026-11-20":           "2026-11-20T00:00:00-05:00",
		"2026-11-20T07:15":     "2026-11-20T07:15:00-05:00",
		"nov 20":               "2026-11-20T00:00:00-05:00",
		"January 3rd":          "2027-01-03T00:00:00-05:00",
		"november 4":           "2027-11-04T00:00:00-04:00",
		"in 3 days":            "2026-11-07T15:30:00-05:00",
		"in 2 hours":           "2026-11-04T17:30:00-05:00",
		"in 1 week":            "2026-11-11T15:30:00-05:00",
		"december 25 at 10:30": "2026-12-25T10:30:00-05:00",
		"2026-11-20 at 9pm":    "2026-11-20T21:00:00-05:00",
	} {
		got, err := ParseUntil(text, now, loc)
		if err != nil || got.Format(time.RFC3339) != want {
			t.Errorf("%q = %s, %v; want %s", text, got.Format(time.RFC3339), err, want)
		}
	}
	for _, text := range []string{"today", "2026-11-01", "today at 9am", "soon", "feb 30", "2026-02-30", "in 0 days", "mon", "next month", "monday at 25:00"} {
		if got, err := ParseUntil(text, now, loc); err == nil {
			t.Errorf("accepted %q as %s", text, got)
		}
	}
	// 02:30 on the spring-forward date does not exist.
	if _, err := ParseUntil("2027-03-14 at 2:30", now, loc); err == nil {
		t.Error("accepted a time in the gap")
	}
	if _, err := ParseUntil("march 14 at 2:30", now, loc); err == nil {
		t.Error("accepted a time in the gap")
	}
	if _, err := ParseUntil("monday", now, time.Local); err == nil {
		t.Error("accepted the host's zone")
	}
}
