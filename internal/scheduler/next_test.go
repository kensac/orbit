package scheduler

import (
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

func TestNextFires(t *testing.T) {
	off := false
	a := &spec.Agent{Name: "a", Triggers: []spec.Trigger{
		{Schedule: "0 9 * * *"}, {Schedule: "30 7 * * *"},
	}}
	if err := a.SetTimezone("America/New_York"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) // 07:00 in New York (EST since 06:00Z)
	got := NextFires(a, now)
	if len(got) != 2 || got[0].TriggerIndex != 1 || !got[0].At.Equal(time.Date(2026, 11, 1, 12, 30, 0, 0, time.UTC)) ||
		got[1].TriggerIndex != 0 || !got[1].At.Equal(time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)) || got[0].At.Location() != time.UTC {
		t.Fatalf("next = %+v", got)
	}
	a.Enabled = &off
	if got := NextFires(a, now); got == nil || len(got) != 0 {
		t.Fatalf("disabled = %+v", got)
	}
}

func TestNextFiresOfAnRRule(t *testing.T) {
	a := &spec.Agent{Name: "a", Triggers: []spec.Trigger{
		{RRule: "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU", Start: "2026-11-03T09:00"},
	}}
	if err := a.SetTimezone("America/New_York"); err != nil {
		t.Fatal(err)
	}
	got := NextFires(a, time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC))
	if len(got) != 1 || got[0].RRule == "" || got[0].Schedule != "" || !got[0].At.Equal(time.Date(2026, 11, 17, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("next = %+v", got)
	}
}

func TestAPausedTriggerSkipsItsFires(t *testing.T) {
	a := &spec.Agent{Name: "a", Triggers: []spec.Trigger{
		{Schedule: "0 9 * * *", Paused: true},
		{Schedule: "0 9 * * *", Paused: true, PausedUntil: "2026-11-05T09:00"},
		{RRule: "FREQ=DAILY", Start: "2026-11-01T08:00", Paused: true, PausedUntil: "2026-11-04T12:00"},
	}}
	if err := a.SetTimezone("America/New_York"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	got := NextFires(a, now)
	// The first never fires; the third at its first fire after the pause (08:00 on the 5th); the second at its end, which is a fire time itself (09:00 on the 5th).
	if len(got) != 2 || got[0].TriggerIndex != 2 || !got[0].At.Equal(time.Date(2026, 11, 5, 13, 0, 0, 0, time.UTC)) ||
		got[1].TriggerIndex != 1 || !got[1].At.Equal(time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)) {
		t.Fatalf("next = %+v", got)
	}
	if _, err := spec.TriggerSchedule(spec.Trigger{Schedule: "0 9 * * *", PausedUntil: "2026-11-05T09:00"}, time.UTC); err == nil {
		t.Fatal("paused_until without paused loaded")
	}
}
