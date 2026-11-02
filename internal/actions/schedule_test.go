package actions_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/spec"
)

func scheduleParams(t *testing.T, typ, p string) actions.ScheduleParams {
	t.Helper()
	v, err := actions.DecodeSchedule(typ, json.RawMessage(p))
	if err != nil {
		t.Fatalf("%s %s: %v", typ, p, err)
	}
	return v
}

func TestApplySchedule(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 11, 4, 15, 30, 0, 0, ny) // a Wednesday
	a := &spec.Agent{Name: "news"}

	res, err := actions.ApplySchedule(a, "America/New_York", actions.ScheduleAddType, scheduleParams(t, actions.ScheduleAddType, `{"when": "every weekday at 8", "prompt": "headlines"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2026-11-05T08:00:00-05:00", "2026-11-06T08:00:00-05:00", "2026-11-09T08:00:00-05:00"}
	if res.Rule != "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;BYHOUR=8;BYMINUTE=0;BYSECOND=0" || strings.Join(res.NextFires, " ") != strings.Join(want, " ") ||
		len(a.Triggers) != 1 || a.Triggers[0].Prompt != "headlines" || a.Triggers[0].Start != "2026-11-04T00:00:00" {
		t.Fatalf("add = %+v, triggers %+v", res, a.Triggers)
	}
	if res, err = actions.ApplySchedule(a, "America/New_York", actions.ScheduleAddType, scheduleParams(t, actions.ScheduleAddType, `{"when": "30 18 * * 0"}`), now); err != nil || res.Rule != "30 18 * * 0" || res.Triggers[0] != 1 {
		t.Fatalf("cron = %+v %v", res, err)
	}

	// Pausing every schedule until Monday: the merged next fires start at the pause's end.
	res, err = actions.ApplySchedule(a, "America/New_York", actions.SchedulePauseType, scheduleParams(t, actions.SchedulePauseType, `{"until": "until Monday"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"2026-11-09T08:00:00-05:00", "2026-11-10T08:00:00-05:00", "2026-11-11T08:00:00-05:00"}
	if !a.Triggers[0].Paused || a.Triggers[1].PausedUntil != "2026-11-09T00:00" || res.PausedUntil == nil ||
		strings.Join(res.NextFires, " ") != strings.Join(want, " ") || len(res.Triggers) != 2 || res.Rule != "" {
		t.Fatalf("pause = %+v %v, triggers %+v", res, res.NextFires, a.Triggers)
	}
	if _, err = actions.ApplySchedule(a, "America/New_York", actions.ScheduleResumeType, scheduleParams(t, actions.ScheduleResumeType, `{"trigger": 1}`), now); err != nil || a.Triggers[1].Paused || !a.Triggers[0].Paused {
		t.Fatalf("resume = %v %+v", err, a.Triggers)
	}
	res, err = actions.ApplySchedule(a, "America/New_York", actions.ScheduleRemoveType, scheduleParams(t, actions.ScheduleRemoveType, `{"trigger": 0}`), now)
	if err != nil || len(a.Triggers) != 1 || a.Triggers[0].Schedule != "30 18 * * 0" || res.Trigger.Prompt != "headlines" {
		t.Fatalf("remove = %+v %v %+v", res, err, a.Triggers)
	}
	// A paused trigger with no end has no next fires.
	_, _ = actions.ApplySchedule(a, "America/New_York", actions.SchedulePauseType, scheduleParams(t, actions.SchedulePauseType, `{}`), now)
	if res, _ = actions.ApplySchedule(a, "America/New_York", actions.SchedulePauseType, scheduleParams(t, actions.SchedulePauseType, `{"trigger": 0}`), now); len(res.NextFires) != 0 {
		t.Fatalf("paused fires = %v", res.NextFires)
	}

	for typ, p := range map[string]string{
		actions.ScheduleAddType:    `{"when": "every so often"}`,
		actions.SchedulePauseType:  `{"until": "eventually"}`,
		actions.ScheduleRemoveType: `{"trigger": 3}`,
	} {
		if _, err := actions.ApplySchedule(a, "America/New_York", typ, scheduleParams(t, typ, p), now); err == nil {
			t.Errorf("%s %s applied", typ, p)
		}
	}
	for typ, p := range map[string]string{
		actions.ScheduleAddType:    `{}`,
		actions.ScheduleRemoveType: `{}`,
		actions.ScheduleResumeType: `{"until": "monday"}`,
		actions.RunNowType:         `{"when": "now"}`,
		actions.SchedulePauseType:  `{"trigger": -1}`,
	} {
		if _, err := actions.DecodeSchedule(typ, json.RawMessage(p)); err == nil {
			t.Errorf("%s %s decoded", typ, p)
		}
	}
}

func TestScheduleTypesAreOfferedOnlyWhenNamed(t *testing.T) {
	a := &spec.Agent{Name: "a", Actions: spec.ActionSet{"schedule:add": spec.RuleApprove}}
	var names []string
	for _, ty := range actions.Catalog(a, spec.Deployment{}) {
		names = append(names, ty.Name)
	}
	if strings.Join(names, ",") != "schedule:add" {
		t.Fatalf("catalog = %v", names)
	}
}
