package spec

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCalendarSkipValidation(t *testing.T) {
	valid := CalendarSkip{
		Connector: "gws", Tool: "list_events", WindowBefore: Duration(15 * time.Minute),
		WindowAfter: Duration(time.Hour), Arguments: map[string]any{
			"timeMin": calendarWindowStart, "timeMax": calendarWindowEnd,
		},
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*CalendarSkip){
		"invalid connector": func(q *CalendarSkip) { q.Connector = "GWS" },
		"invalid tool":      func(q *CalendarSkip) { q.Tool = "bad tool" },
		"negative window":   func(q *CalendarSkip) { q.WindowBefore = Duration(-time.Minute) },
		"empty after":       func(q *CalendarSkip) { q.WindowAfter = 0 },
		"too wide":          func(q *CalendarSkip) { q.WindowAfter = Duration(24*time.Hour - 14*time.Minute) },
		"missing start":     func(q *CalendarSkip) { delete(q.Arguments, "timeMin") },
		"bad template":      func(q *CalendarSkip) { q.Arguments["timeMin"] = "{{today}}" },
		"env reference":     func(q *CalendarSkip) { q.Arguments["timeMin"] = "${TOKEN}" },
		"bad path":          func(q *CalendarSkip) { q.EventsPath = "items[0]" },
	} {
		t.Run(name, func(t *testing.T) {
			q := valid
			q.Arguments = map[string]any{"timeMin": calendarWindowStart, "timeMax": calendarWindowEnd}
			change(&q)
			if err := q.Validate(); err == nil {
				t.Fatal("invalid calendar configuration was accepted")
			}
		})
	}
}

func TestCalendarSkipRenderArguments(t *testing.T) {
	q := CalendarSkip{Arguments: map[string]any{
		"range": map[string]any{"from": calendarWindowStart, "to": calendarWindowEnd},
		"fixed": "primary",
	}}
	start := time.Date(2026, 11, 3, 13, 0, 0, 0, time.FixedZone("offset", -5*60*60))
	got, err := q.RenderArguments(start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if err := json.Unmarshal(got, &args); err != nil {
		t.Fatal(err)
	}
	rangeArgs := args["range"].(map[string]any)
	if rangeArgs["from"] != "2026-11-03T18:00:00Z" || rangeArgs["to"] != "2026-11-03T19:00:00Z" {
		t.Fatalf("rendered range = %#v", rangeArgs)
	}
	if q.Arguments["range"].(map[string]any)["from"] != calendarWindowStart {
		t.Fatal("render modified the configured arguments")
	}
	if strings.Contains(string(got), "${") {
		t.Fatalf("environment reference leaked into rendered args: %s", got)
	}
}
