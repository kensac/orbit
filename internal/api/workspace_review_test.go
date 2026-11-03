package api_test

import (
	"strings"
	"testing"
)

func TestWorkspaceReviewComments(t *testing.T) {
	v := newEnv(t, fastWorkspace)
	withChatAgent(t, v)
	k := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	_, r := call(t, "POST", base+"/agents/chat/conversations", k, `{"body":"hi","wake":true}`)
	conv := r["conversation_id"].(string)
	cu := base + "/conversations/" + conv
	old := "package main\n\nfunc main() {\n\ta()\n\tb()\n}\n"
	finishRun(t, v, conv, map[string][]byte{"main.go": []byte(old), "img.bin": []byte("a\x00")})
	_, _ = call(t, "POST", cu+"/messages", k, `{"body":"change it","wake":true}`)
	run := finishRun(t, v, conv, map[string][]byte{"main.go": []byte(strings.Replace(old, "\tb()\n", "\tB()\n\tc()\n", 1)),
		"img.bin": []byte("a\x00"), "new.txt": []byte("hello\n")})

	add := func(body string) (int, map[string]any) {
		t.Helper()
		return call(t, "POST", cu+"/comments", k, body)
	}
	code, r := add(`{"path":"main.go","start_line":5,"end_line":6,"anchor":{"kind":"run","run_id":"` + run + `"},"body":"Why two calls?"}`)
	c1, _ := r["comment"].(map[string]any)
	if code != 201 || c1["status"] != "draft" || c1["quote"] != "\tB()\n\tc()\n" || c1["side"] != "new" || c1["author"] != "kanishk" ||
		c1["anchor"].(map[string]any)["run_id"] != run || c1["ref"] == nil {
		t.Fatalf("comment = %d %v", code, r)
	}
	code, r = add(`{"path":"main.go","start_line":5,"end_line":5,"side":"old","anchor":{"kind":"run","run_id":"` + run + `"},"body":"This was fine."}`)
	if c2, _ := r["comment"].(map[string]any); code != 201 || c2["quote"] != "\tb()\n" {
		t.Fatalf("old side = %d %v", code, r)
	}
	code, r = add(`{"path":"new.txt","start_line":0,"end_line":0,"anchor":{"kind":"worktree"},"body":"Drop this file."}`)
	c3, _ := r["comment"].(map[string]any)
	if code != 201 || c3["quote"] != "" {
		t.Fatalf("whole file = %d %v", code, r)
	}
	for _, bad := range []struct {
		body, check string
		code        int
	}{
		{`{"path":"main.go","start_line":1,"end_line":1,"anchor":{"kind":"git","repo":"orbit","base":"HEAD","head":"worktree"},"body":"x"}`, "no_git", 409},
		{`{"path":"main.go","start_line":7,"end_line":9,"anchor":{"kind":"worktree"},"body":"x"}`, "request", 400},
		{`{"path":"nope.go","start_line":1,"end_line":1,"anchor":{"kind":"worktree"},"body":"x"}`, "not_found", 404},
		{`{"path":"new.txt","start_line":1,"end_line":1,"side":"old","anchor":{"kind":"run","run_id":"` + run + `"},"body":"x"}`, "not_found", 404},
		{`{"path":"main.go","start_line":1,"end_line":1,"side":"old","anchor":{"kind":"worktree"},"body":"x"}`, "request", 400},
		{`{"path":"img.bin","start_line":1,"end_line":1,"anchor":{"kind":"worktree"},"body":"x"}`, "request", 400},
		{`{"path":"main.go","start_line":2,"end_line":1,"anchor":{"kind":"worktree"},"body":"x"}`, "request", 400},
		{`{"path":"main.go","start_line":1,"end_line":1,"anchor":{"kind":"worktree"},"body":" "}`, "request", 400},
		{`{"path":"../x","start_line":1,"end_line":1,"anchor":{"kind":"worktree"},"body":"x"}`, "path", 400},
		{`{"path":"main.go","start_line":1,"end_line":1,"anchor":{"kind":"run","run_id":"00000000-0000-0000-0000-000000000000"},"body":"x"}`, "not_found", 404},
	} {
		if code, r := add(bad.body); code != bad.code || r["check"] != bad.check {
			t.Fatalf("%s = %d %v, want %d %s", bad.body, code, r, bad.code, bad.check)
		}
	}

	// An edit is a new version; the old one is superseded and edited once.
	code, r = call(t, "POST", cu+"/comments/"+c1["id"].(string)+"/edit", k, `{"body":"Why the second call?","start_line":6,"end_line":6}`)
	e1, _ := r["comment"].(map[string]any)
	if code != 201 || e1["supersedes"] != c1["id"] || e1["quote"] != "\tc()\n" || e1["status"] != "draft" {
		t.Fatalf("edit = %d %v", code, r)
	}
	if code, r := call(t, "POST", cu+"/comments/"+c1["id"].(string)+"/edit", k, `{"body":"again"}`); code != 409 || r["check"] != "state" {
		t.Fatalf("edit twice = %d %v", code, r)
	}
	if code, r := call(t, "POST", cu+"/comments/"+c3["id"].(string)+"/withdraw", k, ""); code != 200 || r["ok"] != true {
		t.Fatalf("withdraw = %d %v", code, r)
	}
	if code, r := call(t, "POST", cu+"/comments/"+c3["id"].(string)+"/withdraw", k, ""); code != 409 || r["check"] != "state" {
		t.Fatalf("withdraw twice = %d %v", code, r)
	}
	_, r = call(t, "GET", cu+"/comments?status=draft", k, "")
	if n := len(r["comments"].([]any)); n != 2 {
		t.Fatalf("drafts = %v", r)
	}
	if code, r := call(t, "GET", cu+"/comments?status=nope", k, ""); code != 400 || r["check"] != "request" {
		t.Fatalf("bad status = %d %v", code, r)
	}

	// The review is one message, formatted by code, and wakes the agent.
	if code, r := call(t, "POST", cu+"/review", k, `{"comments":["`+c3["id"].(string)+`"]}`); code != 400 || r["check"] != "request" {
		t.Fatalf("a withdrawn comment named = %d %v", code, r)
	}
	code, r = call(t, "POST", cu+"/review", k, `{"note":"Two things.","client_request_id":"rev-1"}`)
	msg, _ := r["message_id"].(string)
	if code != 201 || msg == "" || r["run_id"] == nil {
		t.Fatalf("review = %d %v", code, r)
	}
	if code, again := call(t, "POST", cu+"/review", k, `{"note":"Two things.","client_request_id":"rev-1"}`); code != 201 || again["message_id"] != msg {
		t.Fatalf("a retried review = %d %v", code, again)
	}
	if code, r := call(t, "POST", cu+"/review", k, `{}`); code != 400 || r["check"] != "request" {
		t.Fatalf("nothing to send = %d %v", code, r)
	}
	_, r = call(t, "GET", cu+"/thread", k, "")
	var body string
	for _, it := range r["items"].([]any) {
		if it := it.(map[string]any); it["id"] == msg {
			body = it["body"].(string)
		}
	}
	want := "Review of the workspace: 2 comments.\n\nTwo things.\n\n" +
		"1. `main.go`, line 5, on the changes of run " + run[:8] + ", before the run\n   | \tb()\n   > This was fine.\n\n" +
		"2. `main.go`, line 6, on the changes of run " + run[:8] + "\n   | \tc()\n   > Why the second call?\n"
	if body != want {
		t.Fatalf("message:\n%s\nwant\n%s", body, want)
	}
	_, r = call(t, "GET", cu+"/comments?status=sent", k, "")
	if n := len(r["comments"].([]any)); n != 2 {
		t.Fatalf("sent = %v", r)
	}
	// Delivered once a run that read the message succeeded.
	reader := finishRun(t, v, conv, nil)
	_, r = call(t, "GET", cu+"/comments?status=delivered", k, "")
	list := r["comments"].([]any)
	if len(list) != 2 || list[0].(map[string]any)["delivered_by"] != reader || list[0].(map[string]any)["message_id"] != msg {
		t.Fatalf("delivered = %v", r)
	}
	// The audit has a review row per comment.
	_, r = call(t, "GET", cu+"/workspace", k, "")
	_, r = call(t, "GET", base+"/workspaces/"+r["workspace"].(map[string]any)["id"].(string)+"/events", k, "")
	reviews := 0
	for _, e := range r["events"].([]any) {
		if e := e.(map[string]any); e["op"] == "review" && e["path"] == "main.go" && e["person"] == "kanishk" {
			reviews++
		}
	}
	if reviews != 2 {
		t.Fatalf("review events = %v", r["events"])
	}

	// A conversation without a workspace has no comments.
	_, r = call(t, "POST", base+"/agents/a/conversations", k, `{"title":"plain"}`)
	if code, r := call(t, "GET", base+"/conversations/"+r["conversation_id"].(string)+"/comments", k, ""); code != 404 || r["check"] != "no_workspace" {
		t.Fatalf("no workspace = %d %v", code, r)
	}
}
