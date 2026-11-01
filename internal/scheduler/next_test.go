package scheduler

import (
	"testing"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

func TestNextFires(t *testing.T) {
	off := false
	a := &spec.Agent{Name: "a", Timezone: "America/New_York", Triggers: []spec.Trigger{
		{Schedule: "0 9 * * *"}, {Schedule: "30 7 * * *"},
	}}
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
