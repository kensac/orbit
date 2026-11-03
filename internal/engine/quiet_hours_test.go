package engine

import (
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

func TestQuietHoursGate(t *testing.T) {
	a := &spec.Agent{QuietHours: &spec.QuietHours{Start: "22:00", End: "07:00"}}
	if err := a.SetTimezone("America/New_York"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) // 07:00 local.
	quietFire := time.Date(2026, 11, 1, 23, 0, 0, 0, a.Location())
	index := 0
	for _, tc := range []struct {
		name string
		run  store.ClaimedRun
		at   time.Time
		want bool
	}{
		{"scheduled fire is quiet", store.ClaimedRun{Trigger: "schedule", Attempt: 1, TriggerIndex: &index, ScheduledFor: &quietFire}, now, true},
		{"first claim is quiet", store.ClaimedRun{Trigger: "schedule", Attempt: 1}, quietFire, true},
		{"outside quiet hours", store.ClaimedRun{Trigger: "schedule", Attempt: 1}, now, false},
		{"manual run is unaffected", store.ClaimedRun{Trigger: "manual", Attempt: 1, ScheduledFor: &quietFire}, quietFire, false},
		{"retry is unaffected", store.ClaimedRun{Trigger: "schedule", Attempt: 2, ScheduledFor: &quietFire}, quietFire, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := quietHours(a, &tc.run, tc.at) != nil
			if got != tc.want {
				t.Fatalf("quietHours = %v, want %v", got, tc.want)
			}
		})
	}
}
