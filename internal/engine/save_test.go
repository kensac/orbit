package engine_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

func toolUse(id, name, input string) runner.Event {
	return runner.Event{Kind: "assistant", Payload: []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":` + input + `}]}}`)}
}

// TestSaveThisConversationAsAnAgent: the model writes the words; what the agent may do is what the conversation did, and it waits for a person as a dry-run diff.
func TestSaveThisConversationAsAnAgent(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harnessWith(t, fr, "")
	if _, err := e.DefineAgent(ctx, engine.DefineRequest{Spec: json.RawMessage(`{"name": "helper", "charter": "Help.", "wake_on_message": true,
		"actions": {"agent:save": "auto", "webhook:ep": "approve", "notify": "approve", "schedule:add": "approve"}}`), Author: "kanishk"}); err != nil {
		t.Fatal(err)
	}

	// The conversation: it fetched a page, read, ran a command (not allowed unsandboxed, so not carried), used orbit's own tool; the person approved one webhook and rejected a notify; a schedule was asked for.
	fr.events = []runner.Event{
		toolUse("t1", "WebFetch", `{"url": "https://www.news.example.com/today?x=1"}`),
		toolUse("t2", "Read", `{"file_path": "inputs/a.json"}`),
		toolUse("t3", "mcp__orbit__query", `{"sql": "select 1"}`),
		toolUse("t4", "Bash", `{"command": "ls"}`),
		toolUse("t5", "WebFetch", `{"url": "https://www.news.example.com/today?x=1"}`),
	}
	acts, _ := actRun(t, e, s, fr, "helper", "message",
		`{"type": "webhook:ep", "reason": "post it", "params": {"body": {}}}`,
		`{"type": "notify", "reason": "ping", "params": {"channel": "hook", "title": "t", "body": "b"}}`,
		`{"type": "schedule:add", "reason": "asked", "params": {"when": "every weekday at 8", "prompt": "the news"}}`)
	if err := s.DecideAction(ctx, acts[0].ID, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideAction(ctx, acts[1].ID, false, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}

	fr.events = nil
	acts, _ = actRun(t, e, s, fr, "helper", "message",
		`{"type": "agent:save", "reason": "you asked", "params": {"name": "morning", "description": "Morning news.", "charter": "Summarize the news page.", "starters": ["What is new today?"]}}`,
		`{"type": "schedule:add", "reason": "asked", "params": {"agent": "morning", "when": "daily at 18:30"}}`,
		`{"type": "agent:save", "reason": "dup", "params": {"name": "helper", "description": "x", "charter": "y"}}`)
	if len(acts) != 2 || acts[0].Type != "agent:define" || acts[0].Rule != "approve" || acts[0].Status != "pending" || acts[1].Status != "denied" || !strings.Contains(*acts[1].Error, "exists") {
		t.Fatalf("proposals = %+v", acts)
	}
	var p struct {
		Spec spec.Agent `json:"spec"`
	}
	if err := json.Unmarshal(acts[0].Params, &p); err != nil {
		t.Fatal(err)
	}
	a := p.Spec
	rules := map[string]spec.Rule{"webhook:ep": spec.RuleApprove}
	if a.Name != "morning" || a.Charter != "Summarize the news page." || !slices.Equal(a.Starters, []string{"What is new today?"}) ||
		!slices.Equal(a.Tools, []string{"WebFetch", "Read"}) || len(a.Sources) != 1 || a.Sources[0].Name != "news-example-com" ||
		a.Sources[0].URL != "https://www.news.example.com/today?x=1" || len(a.Actions) != 1 || a.Actions["webhook:ep"] != rules["webhook:ep"] ||
		len(a.Triggers) != 2 || a.Triggers[0].Prompt != "the news" || !strings.Contains(a.Triggers[1].RRule, "BYHOUR=18;BYMINUTE=30") ||
		!a.MessagesTrigger || a.Sandbox != "" {
		t.Fatalf("drafted = %+v", a)
	}

	// The person sees the whole agent as a diff, and what it adds.
	pending := action(t, s, acts[0].ID)
	pv, err := e.PreviewAction(ctx, &pending)
	if err != nil {
		t.Fatal(err)
	}
	var prev struct {
		Agent, Change, Diff string
		Widens              []string
	}
	_ = json.Unmarshal(pv, &prev)
	if prev.Agent != "morning" || prev.Change != "create" || !strings.Contains(prev.Diff, `+   "charter": "Summarize the news page.",`) ||
		strings.Contains(prev.Diff, "\n- ") || !slices.Contains(prev.Widens, "tool WebFetch") {
		t.Fatalf("preview = %s", pv)
	}
	if err := s.DecideAction(ctx, acts[0].ID, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	m := e.Current().Config.Agents["morning"]
	if m == nil || m.Origin != spec.OriginDatabase || len(m.Triggers) != 2 {
		t.Fatalf("morning = %+v (%+v)", m, action(t, s, acts[0].ID))
	}
	if h, _ := s.DefinitionHistory(ctx, "morning"); len(h) != 1 || h[0].Author == nil || *h[0].Author != "kanishk" {
		t.Fatalf("history = %+v", h)
	}
	// The schedule asked for the new agent went into it, not out as an action of its own.
	for _, act := range runActions(t, s, acts[0].RunID) {
		if act.Type == "schedule:add" {
			t.Fatalf("absorbed schedule proposed too: %+v", act)
		}
	}
}

func TestADefinitionPreviewIsADiffOfTheChange(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harnessWith(t, fr, "")
	if _, err := e.DefineAgent(ctx, engine.DefineRequest{Spec: json.RawMessage(`{"name": "news", "charter": "Old."}`)}); err != nil {
		t.Fatal(err)
	}
	pv, err := e.PreviewAction(ctx, &store.Action{Status: "pending", Type: "agent:define", Params: json.RawMessage(`{"spec": {"name": "news", "charter": "New."}}`)})
	if err != nil {
		t.Fatal(err)
	}
	var prev struct{ Change, Diff string }
	_ = json.Unmarshal(pv, &prev)
	if prev.Change != "update" || !strings.Contains(prev.Diff, `-   "charter": "Old.",`) || !strings.Contains(prev.Diff, `+   "charter": "New.",`) || !strings.Contains(prev.Diff, `    "name": "news",`) {
		t.Fatalf("preview = %s", pv)
	}
	if _, err := e.PreviewAction(ctx, &store.Action{Status: "pending", Type: "agent:define", Params: json.RawMessage(`{"spec": {"name": "news", "charterz": 1}}`)}); err == nil {
		t.Fatal("a spec that does not load previewed")
	}
	_ = s
}
