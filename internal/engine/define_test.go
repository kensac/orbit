package engine_test

import (
	"encoding/json"
	"testing"

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
