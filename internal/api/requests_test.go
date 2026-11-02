package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
)

// TestDecideRejectsAMalformedBody: a body approve or reject cannot read is a 400 and decides nothing, so a client_request_id is never silently dropped from a decision.
func TestDecideRejectsAMalformedBody(t *testing.T) {
	srv, s, e := server(t)
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: e.Current().Config.Agents["a"].SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	c, _ := s.ClaimRun(ctx, "w")
	ids, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{
			{Type: "notify", Params: json.RawMessage(`{"channel":"c","title":"t","body":"b"}`), Reason: "one", Rule: "approve"},
			{Type: "notify", Params: json.RawMessage(`{"channel":"c","title":"t","body":"b"}`), Reason: "two", Rule: "approve"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/approve", "/reject"} {
		for _, body := range []string{`{"client_request_id":`, `{"client_requst_id":"r1"}`, `"r1"`} {
			if code, v := call(t, "POST", srv.URL+"/api/v1/actions/"+ids[0]+path, token, body); code != 400 {
				t.Fatalf("%s %s = %d %v, want 400", path, body, code, v)
			}
		}
	}
	if a, _ := s.GetAction(ctx, ids[0]); a.Status != "pending" {
		t.Fatalf("status after bad bodies = %s, want pending", a.Status)
	}
	// A good body decides, and its client_request_id makes a retry return the first result.
	for range 2 {
		if code, v := call(t, "POST", srv.URL+"/api/v1/actions/"+ids[0]+"/approve", token, `{"client_request_id":"r1"}`); code != 200 {
			t.Fatalf("approve = %d %v", code, v)
		}
	}
	// An empty body is still accepted.
	if code, v := call(t, "POST", srv.URL+"/api/v1/actions/"+ids[1]+"/reject", token, ""); code != 200 {
		t.Fatalf("reject with no body = %d %v", code, v)
	}
}

// TestUnknownBodyFieldsAre400: handlers that once decoded leniently now refuse a field they do not know.
func TestUnknownBodyFieldsAre400(t *testing.T) {
	srv, s, _ := server(t)
	conv, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/agents/a/runs", `{"prompt":"x","promt":"y"}`},
		{"POST", "/agents/a/messages", `{"body":"x","client_msg_id":"y"}`},
		{"POST", "/agents/a/conversations", `{"title":"x","colour":"red"}`},
		{"PATCH", "/conversations/" + conv.ConversationID, `{"title":"x","pinned":true}`},
		{"POST", "/agents", `{"spec":{},"dryrun":true}`},
		{"POST", "/conversations/" + conv.ConversationID + "/uploads/url", `{"name":"a.txt","size":3}`},
	} {
		code, v := call(t, c.method, srv.URL+"/api/v1"+c.path, token, c.body)
		if code != 400 || !strings.Contains(v["error"].(string), "unknown field") {
			t.Errorf("%s %s = %d %v, want 400 for an unknown field", c.method, c.path, code, v)
		}
	}
}

// TestQueryIntegersAre400: a limit, after, attempt or offset that is not an integer is the client's mistake, not a silent default.
func TestQueryIntegersAre400(t *testing.T) {
	srv, _, _ := server(t)
	run := "0b5f3c2e-8f6a-4d3b-9c1e-2a7f4e6d8b90"
	for _, path := range []string{
		"/runs?limit=ten",
		"/runs/" + run + "/events?after=x",
		"/runs/" + run + "/events?attempt=1.5",
		"/runs/" + run + "/events?limit=many",
		"/runs/" + run + "/journal?limit=x",
		"/actions?limit=x",
		"/agents/a/memory?limit=x",
		"/agents/a/messages?limit=x",
		"/agents/a/thread?limit=x",
		"/agents/a/conversations?limit=x",
		"/memory/entries?limit=x",
		"/tickets?limit=x",
		"/search?q=a&offset=x",
	} {
		code, v := call(t, "GET", srv.URL+"/api/v1"+path, token, "")
		if code != 400 || !strings.Contains(v["error"].(string), "want an integer") {
			t.Errorf("%s = %d %v, want 400", path, code, v)
		}
	}
}
