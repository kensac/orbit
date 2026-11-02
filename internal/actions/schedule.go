package actions

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/scheduletext"
	"github.com/kensac/orbit/internal/spec"
)

// The schedule actions change one agent's triggers without rewriting its spec: each executes as a new version of that agent through agent:define's path (revertible, with the run and the person on it). Times are parsed here, by code, in the agent's timezone; the model passes the person's words.
const (
	ScheduleAddType    = "schedule:add"
	SchedulePauseType  = "schedule:pause"
	ScheduleResumeType = "schedule:resume"
	ScheduleRemoveType = "schedule:remove"
	// RunNowType starts a run of an agent now; it changes no spec.
	RunNowType = "run:now"
)

// ScheduleTypes are the schedule actions and run:now: in a run answering its person's message, approve on them applies directly, as their words are the instruction.
var ScheduleTypes = []string{ScheduleAddType, SchedulePauseType, ScheduleResumeType, ScheduleRemoveType, RunNowType}

// IsScheduleType reports whether t is one of ScheduleTypes.
func IsScheduleType(t string) bool { return slices.Contains(ScheduleTypes, t) }

var agentNameRE = regexp.MustCompile(spec.AgentNamePattern)

// FiresShown is how many next fires a schedule action echoes for the person to confirm.
const FiresShown = 3

// maxWhen bounds a schedule phrase: the longest supported one is under 60 characters.
const maxWhen = 200

// ScheduleParams are a schedule action's params; which fields apply depends on the type.
type ScheduleParams struct {
	// Agent is the agent changed: this one when empty, else one it may delegate to.
	Agent string `json:"agent,omitempty"`
	// When is the recurrence, as the person said it ("every weekday at 8", "the 1st at 9am") or a cron expression (schedule:add).
	When string `json:"when,omitempty"`
	// Prompt is the reason for the wake (schedule:add) or the run (run:now).
	Prompt string `json:"prompt,omitempty"`
	// Trigger is the index of the trigger changed, as next_fires reports it; pause and resume without it change every trigger.
	Trigger *int `json:"trigger,omitempty"`
	// Until ends a pause, as the person said it ("monday", "until nov 9", "in 3 days").
	Until string `json:"until,omitempty"`
}

// DecodeSchedule reads and checks the shape of a schedule action's params; what needs the agent (its timezone, its triggers) is checked by ApplySchedule.
func DecodeSchedule(typ string, p json.RawMessage) (ScheduleParams, error) {
	var v ScheduleParams
	if err := strictDecode(p, &v); err != nil {
		return v, err
	}
	if v.Agent != "" && !agentNameRE.MatchString(v.Agent) {
		return v, fmt.Errorf("%s: agent %q is not an agent name", typ, v.Agent)
	}
	if v.Trigger != nil && *v.Trigger < 0 {
		return v, fmt.Errorf("%s: trigger must be 0 or more", typ)
	}
	if len(v.When) > maxWhen || len(v.Until) > maxWhen || len(v.Prompt) > 4000 {
		return v, fmt.Errorf("%s: when and until are at most %d characters, prompt 4000", typ, maxWhen)
	}
	allowed := map[string][]string{
		ScheduleAddType:    {"agent", "when", "prompt"},
		SchedulePauseType:  {"agent", "trigger", "until"},
		ScheduleResumeType: {"agent", "trigger"},
		ScheduleRemoveType: {"agent", "trigger"},
		RunNowType:         {"agent", "prompt"},
	}[typ]
	set := map[string]bool{"agent": v.Agent != "", "when": v.When != "", "prompt": v.Prompt != "", "trigger": v.Trigger != nil, "until": v.Until != ""}
	for k, on := range set {
		if on && !slices.Contains(allowed, k) {
			return v, fmt.Errorf("%s does not take %s", typ, k)
		}
	}
	switch {
	case typ == ScheduleAddType && strings.TrimSpace(v.When) == "":
		return v, fmt.Errorf("%s: when is required", typ)
	case typ == ScheduleRemoveType && v.Trigger == nil:
		return v, fmt.Errorf("%s: trigger is required", typ)
	}
	return v, nil
}

func scheduleType(typ string, a *spec.Agent) Type {
	agent := map[string]any{"type": "string", "pattern": spec.AgentNamePattern,
		"description": "The agent to change; omit for yourself (\"" + a.Name + "\")."}
	trigger := map[string]any{"type": "integer", "minimum": 0, "description": "The trigger's index, as next_fires lists it."}
	props := map[string]any{"agent": agent}
	var desc string
	var required []string
	switch typ {
	case ScheduleAddType:
		desc = "Add a schedule: when is the person's words (\"every weekday at 8\", \"every other monday at 9am\", \"the 1st at 9am\", \"daily at 18:30\") or a cron expression. " +
			"orbit parses it in the agent's timezone (a bare time is the 24-hour clock); the result shows the rule and the next fires: tell the person both."
		props["when"] = map[string]any{"type": "string", "minLength": 1, "maxLength": maxWhen}
		props["prompt"] = map[string]any{"type": "string", "maxLength": 4000, "description": "What the agent does when woken."}
		required = []string{"when"}
	case SchedulePauseType:
		desc = "Pause a schedule (trigger), or every schedule without one; until, in the person's words (\"monday\", \"nov 9\", \"tomorrow at 9am\", \"in 3 days\"), resumes it then. Fires during the pause are skipped."
		props["trigger"] = trigger
		props["until"] = map[string]any{"type": "string", "maxLength": maxWhen}
	case ScheduleResumeType:
		desc = "Resume a paused schedule (trigger), or every schedule without one."
		props["trigger"] = trigger
	case ScheduleRemoveType:
		desc = "Remove one schedule (trigger). Later triggers' indexes move down by one."
		props["trigger"] = trigger
		required = []string{"trigger"}
	case RunNowType:
		desc = "Start a run of the agent now, with prompt as its reason. A run not woken by a person's message cannot start itself again (that is continue)."
		props["prompt"] = map[string]any{"type": "string", "maxLength": 4000}
	}
	schema := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if required != nil {
		schema["required"] = required
	}
	return Type{Name: typ, Description: desc, ParamsSchema: schema, Validate: func(p json.RawMessage) error {
		_, err := DecodeSchedule(typ, p)
		return err
	}}
}

// ScheduleResult is what a schedule action did, or would do: echoed for the person to confirm.
type ScheduleResult struct {
	Agent    string `json:"agent"`
	Timezone string `json:"timezone"`
	// Triggers are the indexes changed.
	Triggers []int `json:"triggers"`
	// Trigger is the trigger added (schedule:add) or removed (schedule:remove).
	Trigger *spec.Trigger `json:"trigger,omitempty"`
	// Rule is the compiled recurrence: the rrule, or the cron expression.
	Rule string `json:"rule,omitempty"`
	// PausedUntil is when a pause ends, as an instant.
	PausedUntil *time.Time `json:"paused_until,omitempty"`
	// NextFires are the next fires of the changed triggers, merged, soonest first (at most FiresShown), as local times in the agent's timezone.
	NextFires []string `json:"next_fires"`
	// Version is the agent's new version, once applied.
	Version int `json:"version,omitempty"`
}

// ApplySchedule changes a's triggers in place for a schedule action (not run:now) at now, read in loc (the agent's timezone as it loads: a spec may leave it to orbit.yaml's), and says what it did.
func ApplySchedule(a *spec.Agent, loc *time.Location, typ string, p ScheduleParams, now time.Time) (*ScheduleResult, error) {
	res := &ScheduleResult{Agent: a.Name, Timezone: loc.String(), Triggers: []int{}}
	pick := func() ([]int, error) {
		if p.Trigger == nil {
			if len(a.Triggers) == 0 {
				return nil, fmt.Errorf("%s: agent %q has no schedules", typ, a.Name)
			}
			out := make([]int, len(a.Triggers))
			for i := range out {
				out[i] = i
			}
			return out, nil
		}
		if *p.Trigger >= len(a.Triggers) {
			return nil, fmt.Errorf("%s: agent %q has %d schedules; trigger %d is not one", typ, a.Name, len(a.Triggers), *p.Trigger)
		}
		return []int{*p.Trigger}, nil
	}
	switch typ {
	case ScheduleAddType:
		t, err := compileWhen(p.When, now, loc)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", typ, err)
		}
		t.Prompt = p.Prompt
		a.Triggers = append(a.Triggers, t)
		res.Triggers = []int{len(a.Triggers) - 1}
		res.Trigger = &t
	case SchedulePauseType:
		idx, err := pick()
		if err != nil {
			return nil, err
		}
		until := ""
		if p.Until != "" {
			u, err := scheduletext.ParseUntil(p.Until, now, loc)
			if err != nil {
				return nil, fmt.Errorf("%s: until: %w", typ, err)
			}
			res.PausedUntil = &u
			until = u.Format("2006-01-02T15:04")
		}
		for _, i := range idx {
			a.Triggers[i].Paused, a.Triggers[i].PausedUntil = true, until
		}
		res.Triggers = idx
	case ScheduleResumeType:
		idx, err := pick()
		if err != nil {
			return nil, err
		}
		for _, i := range idx {
			a.Triggers[i].Paused, a.Triggers[i].PausedUntil = false, ""
		}
		res.Triggers = idx
	case ScheduleRemoveType:
		idx, err := pick()
		if err != nil {
			return nil, err
		}
		t := a.Triggers[idx[0]]
		res.Trigger = &t
		a.Triggers = append(a.Triggers[:idx[0]:idx[0]], a.Triggers[idx[0]+1:]...)
		return res, nil
	default:
		return nil, fmt.Errorf("%s is not a schedule change", typ)
	}
	if len(res.Triggers) == 1 {
		t := a.Triggers[res.Triggers[0]]
		res.Rule = t.Schedule + t.RRule
	}
	res.NextFires = nextFires(a.Triggers, res.Triggers, now, loc)
	return res, nil
}

// compileWhen is a phrase as scheduletext reads it, else a cron expression.
func compileWhen(when string, now time.Time, loc *time.Location) (spec.Trigger, error) {
	t, perr := scheduletext.Parse(when, now, loc)
	if perr == nil {
		return t, nil
	}
	expr := strings.TrimSpace(when)
	if _, err := spec.ParseSchedule(expr); err == nil && (strings.HasPrefix(expr, "@") || len(strings.Fields(expr)) == 5) {
		return spec.Trigger{Schedule: expr}, nil
	}
	return spec.Trigger{}, fmt.Errorf("%q is neither a supported phrase (%s) nor a cron expression", when, perr.Error())
}

// nextFires merges the next fires of triggers idx after now, soonest first, at most FiresShown.
func nextFires(ts []spec.Trigger, idx []int, now time.Time, loc *time.Location) []string {
	type sched struct {
		s    spec.Schedule
		next time.Time
	}
	var all []*sched
	for _, i := range idx {
		s, err := spec.TriggerSchedule(ts[i], loc)
		if err != nil {
			continue
		}
		all = append(all, &sched{s: s, next: s.Next(now.In(loc))})
	}
	out := []string{}
	for len(out) < FiresShown {
		var soonest *sched
		for _, s := range all {
			if !s.next.IsZero() && (soonest == nil || s.next.Before(soonest.next)) {
				soonest = s
			}
		}
		if soonest == nil {
			break
		}
		if f := soonest.next.In(loc).Format(time.RFC3339); len(out) == 0 || out[len(out)-1] != f {
			out = append(out, f)
		}
		soonest.next = soonest.s.Next(soonest.next)
	}
	return out
}

// MayTarget reports whether proposer may aim a schedule action at target: itself, or an agent it may delegate to.
func MayTarget(proposer *spec.Agent, target string) bool {
	return target == proposer.Name || slices.Contains(proposer.DelegateTargets(), target)
}
