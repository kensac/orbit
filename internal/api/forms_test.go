package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/forms"
	"github.com/kensac/orbit/internal/store"
)

// TestAQuestionsFormIsAnsweredByCode: a run asks a question with a form; the thread shows the fields on the run and on the question's item; a reply's answers are checked against the form and stored beside a body that lists them for the agent.
func TestAQuestionsFormIsAnsweredByCode(t *testing.T) {
	v := newEnv(t, nil)
	enqueue(t, v)
	run, _ := claim(t, v, "claude-code")
	if _, err := v.s.StartRunContext(ctx, run, "a"); err != nil {
		t.Fatal(err)
	}
	form := []forms.Field{
		{Name: "city", Label: "City", Type: forms.String, Required: true},
		{Name: "budget", Label: "Budget", Type: forms.Number, Default: json.RawMessage(`1500`)},
		{Name: "size", Label: "Size", Type: forms.Select, Options: []string{"studio", "1br"}, Required: true},
		{Name: "letter", Label: "Letter", Type: forms.File},
	}
	if _, err := v.s.RecordOutcome(ctx, run, "a", store.RunResult{Attempt: 1, Status: "succeeded", Summary: "s", Report: "r"}, store.Outcome{
		Questions: []store.Question{{Body: "Where should I look?", Fields: form}, {Body: "Anything else?"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.s.RecordOutcome(ctx, enqueueAndStart(t, v), "a", store.RunResult{Attempt: 1, Status: "succeeded", Summary: "s", Report: "r"}, store.Outcome{
		Questions: []store.Question{{Body: "bad", Fields: []forms.Field{{Name: "x", Type: "color"}}}},
	}); err == nil || !strings.Contains(err.Error(), "question form") {
		t.Fatalf("an invalid form was stored: %v", err)
	}

	p := thread(t, v, "")
	at := -1
	for i, it := range p.Items {
		if it.ID == run {
			at = i
		}
	}
	if at < 0 {
		t.Fatal("the run is not in its thread")
	}
	r := p.Items[at]
	if len(r.Questions) != 2 || len(r.Questions[0].Fields) != 4 || r.Questions[0].Fields[2].Options[1] != "1br" || r.Questions[1].Fields != nil {
		t.Fatalf("run questions = %+v", r.Questions)
	}
	qid := r.Questions[0].ID
	_, raw := get(t, v.srv.URL+"/api/v1/agents/a/thread", v.person("kanishk"))
	var qItem, plain map[string]any
	for _, it := range raw["items"].([]any) {
		m := it.(map[string]any)
		switch m["id"] {
		case qid:
			qItem = m
		case r.Questions[1].ID:
			plain = m
		}
	}
	if f, _ := qItem["fields"].([]any); len(f) != 4 || f[1].(map[string]any)["default"] != 1500.0 {
		t.Fatalf("question item = %v", qItem)
	}
	if _, has := plain["fields"]; has {
		t.Fatalf("a question without a form carries fields: %v", plain)
	}

	conv := v.s.Pool
	var convID string
	if err := conv.QueryRow(ctx, `SELECT conversation_id::text FROM messages WHERE id = $1`, qid).Scan(&convID); err != nil {
		t.Fatal(err)
	}
	up, err := v.s.CreateUpload(ctx, store.NewUpload{ConversationID: convID, Name: "letter.pdf", MediaType: "application/pdf", Content: []byte("%PDF"), Author: "kanishk"})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := v.s.CreateConversation(ctx, store.NewConversation{Agent: "a", Title: "other"})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := v.s.CreateUpload(ctx, store.NewUpload{ConversationID: other.ConversationID, Name: "x.txt", MediaType: "text/plain", Content: []byte("x"), Author: "kanishk"})
	if err != nil {
		t.Fatal(err)
	}
	post := func(body string) (int, map[string]any) {
		return call(t, "POST", v.srv.URL+"/api/v1/agents/a/messages", v.person("kanishk"), body)
	}
	for name, body := range map[string]string{
		"no reply_to":    `{"answers": {"city": "Queens", "size": "1br"}}`,
		"no form":        `{"reply_to": "` + r.Questions[1].ID + `", "answers": {"city": "Queens"}}`,
		"unknown field":  `{"reply_to": "` + qid + `", "answers": {"city": "Queens", "size": "1br", "zip": "11101"}}`,
		"missing":        `{"reply_to": "` + qid + `", "answers": {"size": "1br"}}`,
		"not an option":  `{"reply_to": "` + qid + `", "answers": {"city": "Queens", "size": "2br"}}`,
		"wrong type":     `{"reply_to": "` + qid + `", "answers": {"city": "Queens", "size": "1br", "budget": "lots"}}`,
		"other's upload": `{"reply_to": "` + qid + `", "answers": {"city": "Queens", "size": "1br", "letter": "` + elsewhere.ID + `"}}`,
	} {
		if code, b := post(body); code != 400 || !strings.Contains(b["error"].(string), "answers") {
			t.Errorf("%s: %d %v", name, code, b)
		}
	}
	code, b := post(`{"reply_to": "` + qid + `", "body": "Near a train, please.", "answers": {"city": "Queens", "size": "1br", "letter": "` + up.ID + `"}}`)
	if code != 201 {
		t.Fatalf("answer = %d %v", code, b)
	}
	mid := b["message_id"].(string)
	_, raw = get(t, v.srv.URL+"/api/v1/agents/a/thread", v.person("kanishk"))
	var reply map[string]any
	for _, it := range raw["items"].([]any) {
		if m := it.(map[string]any); m["id"] == mid {
			reply = m
		}
	}
	answers, _ := reply["answers"].(map[string]any)
	if answers["city"] != "Queens" || answers["budget"] != 1500.0 || answers["size"] != "1br" || answers["letter"] != up.ID {
		t.Fatalf("answers = %v", reply)
	}
	wantBody := "Near a train, please.\n\nAnswers to \"Where should I look?\":\n1. City: Queens\n2. Budget: 1500\n3. Size: 1br\n4. Letter: uploads/letter.pdf"
	if reply["body"] != wantBody {
		t.Fatalf("body = %q", reply["body"])
	}
	if att, _ := reply["attachments"].([]any); len(att) != 1 || att[0].(map[string]any)["id"] != up.ID {
		t.Fatalf("the file answer is not attached: %v", reply["attachments"])
	}
}

// enqueueAndStart queues a manual run of a, claims it, and starts its context.
func enqueueAndStart(t *testing.T, v *env) string {
	t.Helper()
	enqueue(t, v)
	id, _ := claim(t, v, "claude-code")
	if _, err := v.s.StartRunContext(ctx, id, "a"); err != nil {
		t.Fatal(err)
	}
	return id
}
