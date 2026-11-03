package budget

import (
	"testing"
	"time"
)

func TestWindowUsesAgentCalendarAndMondayWeeks(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		period string
		now    time.Time
		start  string
		end    string
	}{
		{"day", time.Date(2026, 3, 8, 16, 0, 0, 0, loc), "2026-03-08T05:00:00Z", "2026-03-09T04:00:00Z"},
		{"week", time.Date(2026, 11, 4, 16, 0, 0, 0, loc), "2026-11-02T05:00:00Z", "2026-11-09T05:00:00Z"},
		{"month", time.Date(2026, 11, 4, 16, 0, 0, 0, loc), "2026-11-01T04:00:00Z", "2026-12-01T05:00:00Z"},
	} {
		start, end, err := Window(tc.period, tc.now, loc)
		if err != nil || start.Format(time.RFC3339) != tc.start || end.Format(time.RFC3339) != tc.end {
			t.Errorf("%s window = %s..%s, %v", tc.period, start.Format(time.RFC3339), end.Format(time.RFC3339), err)
		}
	}
	if _, _, err := Window("quarter", time.Now(), loc); err == nil {
		t.Fatal("unknown period accepted")
	}
}

func TestAlertAndReachedUseEitherLimit(t *testing.T) {
	for _, tc := range []struct {
		cost           float64
		runs           int
		alert, reached bool
	}{
		{3.99, 2, false, false},
		{4, 2, true, false},
		{2, 5, true, true},
		{5, 2, true, true},
	} {
		if got := Alert(tc.cost, tc.runs, 5, 5); got != tc.alert {
			t.Errorf("Alert(%v, %d) = %v, want %v", tc.cost, tc.runs, got, tc.alert)
		}
		if got := Reached(tc.cost, tc.runs, 5, 5); got != tc.reached {
			t.Errorf("Reached(%v, %d) = %v, want %v", tc.cost, tc.runs, got, tc.reached)
		}
	}
}
