package engine_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/store"
)

const scheduleRules = `"actions": {"schedule:add": "approve", "schedule:pause": "approve", "schedule:resume": "approve", "schedule:remove": "approve", "run:now": "approve"}`

// actRun runs agent once, woken by trigger ("message" or "manual"), with an output proposing these actions, and returns its actions in order with the run's id.
func actRun(t *testing.T, e *engine.Engine, s *store.Store, fr *fakeRunner, agent, trigger string, acts ...string) ([]store.Action, string) {
	t.Helper()
	fr.output = `{"summary": "s", "report": "r", "notify": {"send": false, "priority": "default", "title": ""},
	  "memory": {"update": false, "content": ""}, "questions": [], "actions": [` + strings.Join(acts, ",") + `]}`
	a := e.Current().Config.Agents[agent]
	if trigger == "message" {
		if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: agent, SpecSHA: a.SHA, Body: "go", Author: "kanishk", Wake: true}); err != nil {
			t.Fatal(err)
		}
	} else if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: agent, SpecSHA: a.SHA, Trigger: trigger}); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	return runActions(t, s, c.ID), c.ID
}

func runActions(t *testing.T, s *store.Store, runID string) []store.Action {
	t.Helper()
	page, err := s.ListActionsPage(ctx, store.ActionFilter{RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	out := page.Actions
	// Newest first in the page; the run proposed them in order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func action(t *testing.T, s *store.Store, id string) store.Action {
	t.Helper()
	a, err := s.GetAction(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return *a
}

func TestScheduleActionsChangeAnAgentAsVersions(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harnessWith(t, fr, "")
	if _, err := e.DefineAgent(ctx, engine.DefineRequest{Spec: json.RawMessage(`{"name": "news", "charter": "Headlines.", "wake_on_message": true, ` + scheduleRules + `}`), Author: "kanishk"}); err != nil {
		t.Fatal(err)
	}

	// In the person's own conversation, approve applies directly; a phrase code cannot read is refused at once, with why.
	acts, runID := actRun(t, e, s, fr, "news", "message",
		`{"type": "schedule:add", "reason": "asked", "params": {"when": "every weekday at 8", "prompt": "headlines"}}`,
		`{"type": "schedule:add", "reason": "asked", "params": {"when": "whenever it rains"}}`,
		`{"type": "schedule:pause", "reason": "asked", "params": {"trigger": 4}}`)
	if acts[0].Rule != "auto" || acts[1].Status != "denied" || !strings.Contains(*acts[1].Error, "neither a supported phrase") ||
		acts[2].Status != "denied" || !strings.Contains(*acts[2].Error, "trigger 4 is not one") {
		t.Fatalf("proposals = %+v", acts)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	added := action(t, s, acts[0].ID)
	var res struct {
		Rule      string   `json:"rule"`
		Timezone  string   `json:"timezone"`
		NextFires []string `json:"next_fires"`
		Version   int      `json:"version"`
		Triggers  []int    `json:"triggers"`
	}
	if err := json.Unmarshal(added.Result, &res); err != nil || added.Status != "executed" {
		t.Fatalf("add = %s %v %s", added.Status, added.Error, added.Result)
	}
	ny, _ := time.LoadLocation("America/New_York")
	if res.Rule != "FREQ=WEEKLY;BYDAY=MO,TU,WE,TH,FR;BYHOUR=8;BYMINUTE=0;BYSECOND=0" || res.Timezone != "America/New_York" || res.Version != 2 || len(res.NextFires) != 3 {
		t.Fatalf("result = %+v", res)
	}
	for _, f := range res.NextFires {
		at, err := time.Parse(time.RFC3339, f)
		if at = at.In(ny); err != nil || at.Hour() != 8 || at.Minute() != 0 || at.Weekday() == time.Saturday || at.Weekday() == time.Sunday || !at.After(time.Now()) {
			t.Fatalf("fire %s: %v", f, err)
		}
	}
	news := e.Current().Config.Agents["news"]
	if len(news.Triggers) != 1 || news.Triggers[0].Prompt != "headlines" || news.Version != 2 {
		t.Fatalf("news = %+v", news)
	}
	h, _ := s.DefinitionHistory(ctx, "news")
	if h[0].RunID == nil || *h[0].RunID != runID || h[0].Author == nil || *h[0].Author != "kanishk" || h[0].EditedBy != "news" {
		t.Fatalf("version 2 = %+v", h[0])
	}

	// Unattended, the same rule waits for a person; approved, the pause lands with its end parsed in the agent's zone.
	acts, _ = actRun(t, e, s, fr, "news", "manual", `{"type": "schedule:pause", "reason": "asked", "params": {"until": "monday"}}`)
	if acts[0].Status != "pending" {
		t.Fatalf("unattended pause = %+v", acts[0])
	}
	if err := s.DecideAction(ctx, acts[0].ID, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	paused := e.Current().Config.Agents["news"].Triggers[0]
	until, err := time.ParseInLocation("2006-01-02T15:04", paused.PausedUntil, ny)
	if err != nil || !paused.Paused || until.Weekday() != time.Monday || until.Hour() != 0 || !until.After(time.Now()) {
		t.Fatalf("paused = %+v %v", paused, err)
	}
	var pres struct {
		NextFires []string `json:"next_fires"`
	}
	_ = json.Unmarshal(action(t, s, acts[0].ID).Result, &pres)
	if first, _ := time.Parse(time.RFC3339, pres.NextFires[0]); first.Before(until) {
		t.Fatalf("a paused trigger fires at %s, before %s", first, until)
	}

	// Every change is a version: reverting puts the unpaused schedule back.
	if _, err := e.ChangeAgentState(ctx, "news", "revert", "kanishk", "", 2); err != nil {
		t.Fatal(err)
	}
	if tr := e.Current().Config.Agents["news"].Triggers[0]; tr.Paused {
		t.Fatalf("reverted = %+v", tr)
	}

	// Remove, then an unattended run asking to run itself again is refused at execution (continue is the bounded way), while the person's own ask enqueues it.
	acts, _ = actRun(t, e, s, fr, "news", "message",
		`{"type": "schedule:remove", "reason": "asked", "params": {"trigger": 0}}`,
		`{"type": "run:now", "reason": "asked", "params": {"prompt": "now please"}}`)
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(e.Current().Config.Agents["news"].Triggers); n != 0 || action(t, s, acts[0].ID).Status != "executed" {
		t.Fatalf("triggers after remove = %d, %+v", n, action(t, s, acts[0].ID))
	}
	var now struct {
		RunID string `json:"run_id"`
	}
	ran := action(t, s, acts[1].ID)
	if _ = json.Unmarshal(ran.Result, &now); ran.Status != "executed" || now.RunID == "" {
		t.Fatalf("run:now = %+v", ran)
	}
	if r, err := s.GetRun(ctx, now.RunID); err != nil || r.Trigger != "manual" || r.Prompt != "now please" {
		t.Fatalf("queued = %+v %v", r, err)
	}
	c, _ := s.ClaimRun(ctx, "test")
	fr.output = `{"summary": "s", "report": "r", "notify": {"send": false, "priority": "default", "title": ""},
	  "memory": {"update": false, "content": ""}, "questions": [], "actions": [{"type": "run:now", "reason": "again", "params": {}}]}`
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	again := runActions(t, s, c.ID)[0]
	if err := s.DecideAction(ctx, again.ID, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	if again = action(t, s, again.ID); again.Status != "failed" || !strings.Contains(*again.Error, "use continue") {
		t.Fatalf("self run:now from a manual run = %+v", again)
	}
}

func TestScheduleActionsReachOnlyWhatTheAgentMayChange(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harnessWith(t, fr, "")
	for _, sp := range []string{`{"name": "other", "charter": "x"}`,
		`{"name": "news", "charter": "x", "wake_on_message": true, "delegates": ["other"], ` + scheduleRules + `}`} {
		if _, err := e.DefineAgent(ctx, engine.DefineRequest{Spec: json.RawMessage(sp), Author: "kanishk"}); err != nil {
			t.Fatal(err)
		}
	}
	acts, _ := actRun(t, e, s, fr, "news", "message",
		`{"type": "schedule:add", "reason": "a", "params": {"agent": "other", "when": "0 7 * * *"}}`,
		`{"type": "schedule:add", "reason": "a", "params": {"agent": "finance", "when": "daily at 7"}}`,
		`{"type": "schedule:add", "reason": "a", "params": {"agent": "orbit", "when": "daily at 7"}}`)
	if acts[0].Rule != "auto" || acts[1].Status != "denied" || acts[2].Status != "denied" || !strings.Contains(*acts[2].Error, "delegates to") {
		t.Fatalf("proposals = %+v", acts)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	if tr := e.Current().Config.Agents["other"].Triggers; len(tr) != 1 || tr[0].Schedule != "0 7 * * *" {
		t.Fatalf("other = %+v (%+v)", tr, action(t, s, acts[0].ID))
	}
	if !strings.Contains(*acts[1].Error, "not finance") && !strings.Contains(*acts[1].Error, "\"finance\"") {
		t.Fatalf("finance: %s", *acts[1].Error)
	}
}

func TestTheBuiltInChangesItsOwnSchedule(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harnessWith(t, fr, "")
	if err := e.Reload(ctx); err != nil || e.Current().Config.Agents["orbit"] == nil {
		t.Fatalf("the built-in is not loaded: %v", err)
	}
	a, err := e.PreviewSchedule(ctx, "orbit", "schedule:add", json.RawMessage(`{"when": "the 1st at 9am"}`))
	if err != nil || a.Rule != "FREQ=MONTHLY;BYMONTHDAY=1;BYHOUR=9;BYMINUTE=0;BYSECOND=0" || len(a.NextFires) != 3 {
		t.Fatalf("preview = %+v %v", a, err)
	}
	if _, err := s.LatestDefinition(ctx, "orbit"); err == nil {
		t.Fatal("a preview stored a definition")
	}
}
