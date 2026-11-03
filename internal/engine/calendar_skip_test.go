package engine

import (
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

func TestCalendarEventOverlaps(t *testing.T) {
	from := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	q := &spec.CalendarSkip{}
	cases := []struct {
		name  string
		event map[string]any
		busy  bool
		bad   bool
	}{
		{"overlap", map[string]any{"start": map[string]any{"dateTime": "2026-11-03T09:30:00Z"}, "end": map[string]any{"dateTime": "2026-11-03T10:30:00Z"}}, true, false},
		{"touches end", map[string]any{"start": map[string]any{"dateTime": "2026-11-03T10:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-03T11:00:00Z"}}, false, false},
		{"touches start", map[string]any{"start": map[string]any{"dateTime": "2026-11-03T08:00:00Z"}, "end": map[string]any{"dateTime": "2026-11-03T09:00:00Z"}}, false, false},
		{"all day", map[string]any{"start": map[string]any{"date": "2026-11-03"}, "end": map[string]any{"date": "2026-11-04"}}, true, false},
		{"cancelled", map[string]any{"status": "cancelled", "start": "bad", "end": "bad"}, false, false},
		{"transparent", map[string]any{"transparency": "transparent", "start": "bad", "end": "bad"}, false, false},
		{"missing end", map[string]any{"start": "2026-11-03T09:30:00Z"}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			busy, err := calendarEventOverlaps(tc.event, q, from, to, time.UTC)
			if (err != nil) != tc.bad || busy != tc.busy {
				t.Fatalf("overlap = %t, %v; want %t, bad=%t", busy, err, tc.busy, tc.bad)
			}
		})
	}
}

func TestCalendarPath(t *testing.T) {
	value := map[string]any{"data": map[string]any{"events": []any{"event"}}}
	if got, ok := calendarPath(value, "data.events"); !ok || len(got.([]any)) != 1 {
		t.Fatalf("path = %#v, %t", got, ok)
	}
	if _, ok := calendarPath(value, "data.missing"); ok {
		t.Fatal("missing path resolved")
	}
}
