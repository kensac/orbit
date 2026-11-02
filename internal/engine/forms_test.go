package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/runner"
)

// TestAQuestionWithAFormIsStoredAndRenderedAsAList: a report run asks one question with a form and one whose form does not validate; the first keeps its fields, the second is kept as a plain question, the notification lists the form as a numbered list.
func TestAQuestionWithAFormIsStoredAndRenderedAsAList(t *testing.T) {
	fr := &fakeRunner{output: `{
  "summary": "Need two things", "report": "r",
  "notify": {"send": true, "priority": "default", "title": ""},
  "memory": {"update": false, "content": ""},
  "questions": [
    {"question": "Which account and month?", "fields": [
      {"name": "account", "label": "Account", "type": "select", "options": ["checking", "savings"], "default": "checking", "required": true},
      {"name": "month", "label": "Month", "type": "date", "options": [], "default": null, "required": false}]},
    {"question": "Broken form?", "fields": [{"name": "x", "label": "", "type": "color", "options": [], "default": null, "required": false}]},
    {"question": "Fields that are no list?", "fields": [{"name": "x", "options": "a"}]},
    "A plain one?"
  ],
  "actions": []}`}
	e, s, h := harness(t, fr)
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	run, _ := s.ThreadRunByID(ctx, c.ID)
	if run.Status != "succeeded" || len(run.Questions) != 4 {
		t.Fatalf("run = %+v", run)
	}
	if q := run.Questions[0]; len(q.Fields) != 2 || q.Fields[0].Options[1] != "savings" || string(q.Fields[0].Default) != `"checking"` || !q.Fields[0].Required {
		t.Fatalf("form = %+v", q)
	}
	if run.Questions[1].Body != "Broken form?" || run.Questions[1].Fields != nil || run.Questions[2].Fields != nil || run.Questions[3].Fields != nil {
		t.Fatalf("an invalid form or a plain question carries fields: %+v", run.Questions[1:])
	}
	h.mu.Lock()
	body := h.messages[0]["body"].(string)
	h.mu.Unlock()
	want := "- Which account and month?\n  1. Account (one of: checking, savings; default: checking; required)\n  2. Month (a date, YYYY-MM-DD)\n- Broken form?\n- Fields that are no list?\n- A plain one?"
	if !strings.Contains(body, want) {
		t.Fatalf("notification body:\n%s\nwant it to hold:\n%s", body, want)
	}
}

// TestAChatAsksWithAForm: the ask tool takes fields, refuses a form that does not validate so the model can fix it, and the question is stored with its form.
func TestAChatAsksWithAForm(t *testing.T) {
	var refused, asked string
	var refusedErr bool
	fr := &fakeRunner{output: `{}`}
	fr.seen = func(req runner.Request) {
		refused, refusedErr = req.Chat.Call(ctx, "ask", json.RawMessage(`{"question": "Size?", "fields": [{"name": "size", "type": "select"}]}`))
		asked, _ = req.Chat.Call(ctx, "ask", json.RawMessage(`{"question": "Size?", "fields": [{"name": "size", "type": "select", "options": ["s", "m"], "required": true}]}`))
	}
	e, s, _ := harnessWith(t, fr, "chat: true\n")
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if !refusedErr || !strings.Contains(refused, "a select has 1 to") || !strings.Contains(asked, "with a form") {
		t.Fatalf("ask answered %q (error %v), then %q", refused, refusedErr, asked)
	}
	run, _ := s.ThreadRunByID(ctx, c.ID)
	if len(run.Questions) != 1 || len(run.Questions[0].Fields) != 1 || run.Questions[0].Fields[0].Options[1] != "m" {
		t.Fatalf("questions = %+v", run.Questions)
	}
}
