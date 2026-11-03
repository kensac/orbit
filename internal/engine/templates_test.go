package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/templates"
)

func templateOutput(params string) string {
	return `{"summary": "s", "report": "r", "notify": {"send": false, "priority": "default", "title": ""},
	  "memory": {"update": false, "content": ""}, "questions": [],
	  "actions": [{"type": "template:create", "params": {"template": "news", "parameters": ` + params + `}, "reason": "you asked for a news agent"}]}`
}

// TestATemplateCreateAsksThePersonThenWaitsAsADefine: a run's template:create without the person's user_prompt value becomes a question with that form (and the proposal is denied, saying so); once the person answers, the same proposal renders with their answer, never the model's, and is recorded as an agent:define waiting for approval, which creates the agent.
func TestATemplateCreateAsksThePersonThenWaitsAsADefine(t *testing.T) {
	fr := &fakeRunner{output: templateOutput(`{"agent": "headlines", "topics": "what the model guessed"}`)}
	e, s, _ := harnessWith(t, fr, "  template:create: approve\n")
	doc, err := templates.Decode([]byte(`{"name": "news", "description": "Morning headlines.",
	  "parameters": [{"name": "agent", "type": "string", "mode": "required"},
	                 {"name": "topics", "label": "Topics", "type": "string", "mode": "user_prompt", "default": "the news"}],
	  "spec": {"name": "{{agent}}", "charter": "Summarize {{topics}}.", "triggers": [{"schedule": "0 8 * * *"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutTemplate(ctx, store.TemplateWrite{Doc: doc, Create: true}, store.TemplateProvenance{Source: store.TemplateByPerson}); err != nil {
		t.Fatal(err)
	}

	first := claim(t, e, s)
	if err := e.Execute(ctx, first); err != nil {
		t.Fatal(err)
	}
	run, _ := s.ThreadRunByID(ctx, first.ID)
	if len(run.Questions) != 1 || !strings.Contains(run.Questions[0].Body, "template news") || len(run.Questions[0].Fields) != 1 ||
		run.Questions[0].Fields[0].Name != "topics" || !run.Questions[0].Fields[0].Required {
		t.Fatalf("questions = %+v", run.Questions)
	}
	if len(run.Actions) != 1 || run.Actions[0].Type != "template:create" || run.Actions[0].Status != "denied" {
		t.Fatalf("actions = %+v", run.Actions)
	}
	if a, _ := s.GetAction(ctx, run.Actions[0].ID); a.Error == nil || !strings.Contains(*a.Error, "asked the person for topics") {
		t.Fatalf("denial = %+v", a)
	}

	q := run.Questions[0].ID
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", Author: "kanishk", ReplyTo: &q,
		Answers: map[string]json.RawMessage{"topics": json.RawMessage(`"rust releases"`)}}); err != nil {
		t.Fatal(err)
	}
	second := claim(t, e, s)
	if err := e.Execute(ctx, second); err != nil {
		t.Fatal(err)
	}
	run, _ = s.ThreadRunByID(ctx, second.ID)
	if len(run.Questions) != 0 || len(run.Actions) != 1 || run.Actions[0].Type != "agent:define" || run.Actions[0].Status != "pending" {
		t.Fatalf("second run = %+v", run)
	}
	var params struct {
		Spec struct {
			Name    string `json:"name"`
			Charter string `json:"charter"`
		} `json:"spec"`
		Template struct {
			Name    string `json:"name"`
			Version int    `json:"version"`
		} `json:"template"`
	}
	if err := json.Unmarshal(run.Actions[0].Params, &params); err != nil {
		t.Fatal(err)
	}
	if params.Spec.Name != "headlines" || params.Spec.Charter != "Summarize rust releases." || params.Template.Name != "news" || params.Template.Version != 1 {
		t.Fatalf("define params = %s", run.Actions[0].Params)
	}
	if err := s.DecideAction(ctx, run.Actions[0].ID, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if err := e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetAction(ctx, run.Actions[0].ID); a.Status != "executed" {
		t.Fatalf("define = %s %v", a.Status, a.Error)
	}
	if a := e.Current().Config.Agents["headlines"]; a == nil || a.Charter != "Summarize rust releases." {
		t.Fatalf("the agent was not created: %+v", a)
	}

	// A parameter the template lacks, or a required one left out, is denied with the reason.
	for params, want := range map[string]string{
		`{"agent": "x", "colour": "red"}`: `no parameter "colour"`,
		`{}`:                              "needs values for agent",
	} {
		fr.output = templateOutput(params)
		c := claim(t, e, s)
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		r, _ := s.ThreadRunByID(ctx, c.ID)
		a, _ := s.GetAction(ctx, r.Actions[0].ID)
		if a.Status != "denied" || a.Error == nil || !strings.Contains(*a.Error, want) {
			t.Fatalf("%s: %s %v", params, a.Status, a.Error)
		}
	}
}
