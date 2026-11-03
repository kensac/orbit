package api_test

import (
	"strings"
	"testing"
)

func TestConversationsOverTheAPI(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	base := v.srv.URL + "/api/v1"

	// The default conversation exists from the start and is the agent's
	// thread and messages.
	code, r := call(t, "GET", base+"/agents/a/conversations", kanishk, "")
	if code != 200 || len(r["conversations"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, r)
	}
	def := r["conversations"].([]any)[0].(map[string]any)
	if def["is_default"] != true || def["title"] != "General" {
		t.Fatalf("default = %v", def)
	}

	create := `{"body":"How much on Uber Eats in October?","author":"mallory","client_request_id":"n1"}`
	code, r = call(t, "POST", base+"/agents/a/conversations", kanishk, create)
	if code != 201 || r["conversation_id"] == nil || r["message_id"] == nil || r["run_id"] == nil {
		t.Fatalf("create = %d %v", code, r)
	}
	id := r["conversation_id"].(string)
	if _, again := call(t, "POST", base+"/agents/a/conversations", kanishk, create); again["conversation_id"] != id || again["run_id"] != r["run_id"] {
		t.Fatalf("retried create = %v", again)
	}
	code, r = call(t, "GET", base+"/conversations/"+id, kanishk, "")
	c := r["conversation"].(map[string]any)
	if code != 200 || c["title"] != "How much on Uber Eats in October?" || c["agent"] != "a" || c["is_default"] != false {
		t.Fatalf("get = %d %v", code, r)
	}
	msgs, _ := v.s.ConversationThread(ctx, id, nil, 0)
	for _, it := range msgs.Items {
		if it.Message != nil && (it.Message.Author == nil || *it.Message.Author != "kanishk") {
			t.Fatalf("author = %v, want the token's person", it.Message.Author)
		}
	}

	// The new conversation is listed first; the default thread does not
	// show its message.
	_, r = call(t, "GET", base+"/agents/a/conversations?limit=1", kanishk, "")
	if r["conversations"].([]any)[0].(map[string]any)["id"] != id || r["has_more"] != true || r["next_before"] == nil {
		t.Fatalf("first page = %v", r)
	}
	_, r = call(t, "GET", base+"/agents/a/conversations?before="+r["next_before"].(string), kanishk, "")
	if len(r["conversations"].([]any)) != 1 || r["has_more"] != false {
		t.Fatalf("second page = %v", r)
	}
	if _, r := call(t, "GET", base+"/agents/a/thread", kanishk, ""); len(r["items"].([]any)) != 0 {
		t.Fatalf("default thread = %v", r)
	}

	code, r = call(t, "POST", base+"/conversations/"+id+"/messages", kanishk, `{"body":"and September?","wake":false}`)
	if code != 201 || r["run_id"] != nil {
		t.Fatalf("message = %d %v", code, r)
	}
	code, r = call(t, "GET", base+"/conversations/"+id+"/thread", kanishk, "")
	if code != 200 || len(r["items"].([]any)) != 3 { // two messages, one run
		t.Fatalf("thread = %d %v", code, r)
	}

	if code, r := call(t, "PATCH", base+"/conversations/"+id, kanishk, `{"title":"Food spend","archived":true}`); code != 200 ||
		r["conversation"].(map[string]any)["title"] != "Food spend" || r["conversation"].(map[string]any)["archived"] != true {
		t.Fatalf("patch = %d %v", code, r)
	}
	if _, r := call(t, "GET", base+"/agents/a/conversations?archived=true", kanishk, ""); len(r["conversations"].([]any)) != 1 {
		t.Fatalf("archived list = %v", r)
	}
	if code, _ := call(t, "PATCH", base+"/conversations/"+def["id"].(string), kanishk, `{"archived":true}`); code != 409 {
		t.Fatalf("archive the default = %d", code)
	}
	if code, _ := call(t, "PATCH", base+"/conversations/"+id, kanishk, `{"title":"`+longTitle+`"}`); code != 400 {
		t.Fatalf("long title = %d", code)
	}
	if code, _ := call(t, "GET", base+"/conversations/00000000-0000-0000-0000-000000000000", kanishk, ""); code != 404 {
		t.Fatalf("unknown = %d", code)
	}
	if code, _ := call(t, "POST", base+"/agents/ghost/conversations", kanishk, `{}`); code != 404 {
		t.Fatalf("unknown agent = %d", code)
	}
	// A reply must be to a message of the same conversation.
	_, r = call(t, "POST", base+"/agents/a/messages", kanishk, `{"body":"general","wake":false}`)
	if code, _ := call(t, "POST", base+"/conversations/"+id+"/messages", kanishk, `{"body":"x","reply_to":"`+r["message_id"].(string)+`"}`); code != 400 {
		t.Fatalf("reply across conversations = %d", code)
	}
	// delivery: a steer to an agent whose runs cannot take one is queued, and says so.
	if code, _ := call(t, "POST", base+"/conversations/"+id+"/messages", kanishk, `{"body":"x","delivery":"now"}`); code != 400 {
		t.Fatalf("an unknown delivery = %d", code)
	}
	code, r = call(t, "POST", base+"/conversations/"+id+"/messages", kanishk, `{"body":"also","delivery":"steer"}`)
	if code != 201 || r["delivered"] != "queued" || r["run_id"] == nil {
		t.Fatalf("a steer to a report agent = %d %v", code, r)
	}
	if _, r = call(t, "POST", base+"/conversations/"+id+"/messages", kanishk, `{"body":"plain","wake":false}`); r["delivered"] != "queued" || r["run_id"] != nil {
		t.Fatalf("a queued message = %v", r)
	}
}

var longTitle = strings.Repeat("x", 300)

func TestAConversationsModelAndEffort(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/a/conversations", kanishk, `{"title":"x"}`)
	id := r["conversation_id"].(string)
	conv := func(body string, want int) map[string]any {
		t.Helper()
		code, r := call(t, "PATCH", base+"/conversations/"+id, kanishk, body)
		if code != want {
			t.Fatalf("PATCH %s = %d %v, want %d", body, code, r, want)
		}
		if want != 200 {
			return nil
		}
		return r["conversation"].(map[string]any)
	}
	if c := conv(`{"model":"sonnet","effort":"high"}`, 200); c["model"] != "sonnet" || c["effort"] != "high" {
		t.Fatalf("set = %v", c)
	}
	// One field at a time leaves the other; null clears.
	if c := conv(`{"effort":"max"}`, 200); c["model"] != "sonnet" || c["effort"] != "max" {
		t.Fatalf("effort only = %v", c)
	}
	if c := conv(`{"model":null}`, 200); c["model"] != nil || c["effort"] != "max" {
		t.Fatalf("model cleared = %v", c)
	}
	if c := conv(`{"effort":""}`, 200); c["effort"] != nil {
		t.Fatalf("effort cleared = %v", c)
	}
	conv(`{"effort":"extreme"}`, 400)
	conv(`{"model":"Opus Max"}`, 400)
	conv(`{"model":3}`, 400)
	if code, _ := call(t, "PATCH", base+"/conversations/00000000-0000-0000-0000-000000000000", kanishk, `{"model":"opus"}`); code != 404 {
		t.Fatalf("unknown conversation = %d", code)
	}
}
