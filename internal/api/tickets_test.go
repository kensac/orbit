package api_test

import (
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
)

func contains(v any, sub string) bool {
	s, _ := v.(string)
	return strings.Contains(s, sub)
}

func TestTicketLifecycleOverTheAPI(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	u := v.srv.URL + "/api/v1/tickets"
	for body, want := range map[string]string{
		`{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/other"}`:     "not in github.repositories",
		`{"kind":"request","title":"t","body":"b","agent":"ghost","repo":"kensac/orbit"}`: "not configured",
		`{"kind":"question","title":"t","body":"b","agent":"a","repo":"kensac/orbit"}`:    "does not name a repository",
		`{"kind":"bug","title":"t","body":"b","agent":"a"}`:                               "kind must be",
		`{"kind":"question","title":" ","body":"b","agent":"a"}`:                          "title and body",
	} {
		if code, r := call(t, "POST", u, kanishk, body); code != 400 || !contains(r["error"], want) {
			t.Errorf("%s = %d %v, want %q", body, code, r, want)
		}
	}
	create := `{"kind":"request","title":"Add a flag","body":"please","agent":"a","repo":"kensac/orbit","author":"mallory","client_request_id":"c1"}`
	code, r := call(t, "POST", u, kanishk, create)
	if code != 201 || r["run_id"] == nil {
		t.Fatalf("create = %d %v, want a run woken on the ticket", code, r)
	}
	id := r["ticket_id"].(string)
	// A retry with the same id files nothing new.
	if code, again := call(t, "POST", u, kanishk, create); code != 201 || again["ticket_id"] != id || again["run_id"] != r["run_id"] {
		t.Fatalf("retry = %d %v", code, again)
	}
	tk, _ := v.s.GetTicket(ctx, id)
	if *tk.Author != "kanishk" {
		t.Fatalf("author = %s, want the token's person", *tk.Author)
	}

	if code, _ := call(t, "POST", u+"/"+id+"/plans/1/approve", kanishk, ""); code != 409 {
		t.Fatalf("approve with no plan = %d", code)
	}
	if _, err := v.s.ProposePlan(ctx, id, "", "the plan"); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/plans/x/approve", kanishk, ""); code != 400 {
		t.Fatalf("bad version = %d", code)
	}
	approve := `{"by":"mallory","client_request_id":"ap1"}`
	for i := 0; i < 2; i++ { // the retry is a success, not a 409
		if code, r := call(t, "POST", u+"/"+id+"/plans/1/approve", kanishk, approve); code != 200 || r["ok"] != true {
			t.Fatalf("approve %d = %d %v", i, code, r)
		}
	}
	code, r = call(t, "GET", u+"/"+id, kanishk, "")
	ticket := r["ticket"].(map[string]any)
	if code != 200 || ticket["status"] != "approved" || len(r["plans"].([]any)) != 1 || len(r["events"].([]any)) != 3 {
		t.Fatalf("get = %d %v", code, r)
	}
	events := r["events"].([]any)
	if events[2].(map[string]any)["by"] != "kanishk" {
		t.Fatalf("approved by %v, want the token's person", events[2])
	}

	code, r = call(t, "GET", u+"/"+id+"/thread?limit=1", kanishk, "")
	if code != 200 || len(r["items"].([]any)) != 1 || r["has_more"] != true || r["next_before"] == nil {
		t.Fatalf("thread page = %d %v", code, r)
	}
	code, r = call(t, "GET", u+"/"+id+"/thread?before="+r["next_before"].(string), kanishk, "")
	if code != 200 || len(r["items"].([]any)) != 1 || r["has_more"] != false {
		t.Fatalf("thread rest = %d %v", code, r)
	}
	// The agent's general thread does not show the ticket.
	if _, r := call(t, "GET", v.srv.URL+"/api/v1/agents/a/thread", kanishk, ""); len(r["items"].([]any)) != 0 {
		t.Fatalf("general thread = %v", r)
	}

	if code, _ := call(t, "POST", u+"/"+id+"/reopen", kanishk, ""); code != 409 {
		t.Fatalf("reopen an approved ticket = %d", code)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/abandon", kanishk, `{"reason":"later"}`); code != 200 {
		t.Fatalf("abandon = %d", code)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/messages", kanishk, `{"body":"more"}`); code != 409 {
		t.Fatalf("message to an abandoned ticket = %d", code)
	}
	if code, _ := call(t, "GET", u+"/00000000-0000-0000-0000-000000000000", kanishk, ""); code != 404 {
		t.Fatalf("unknown ticket = %d", code)
	}
	if code, r := call(t, "GET", u+"?status=abandoned", kanishk, ""); code != 200 || len(r["tickets"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, r)
	}
}

func TestTicketMessagesAndTheOperatorToken(t *testing.T) {
	v := newEnv(t, nil)
	u := v.internal.URL + "/api/v1/tickets"
	_, r := call(t, "POST", u, token, `{"kind":"question","title":"Why?","body":"why is X","agent":"a","author":"script","wake":false}`)
	id := r["ticket_id"].(string)
	if r["run_id"] != nil {
		t.Fatal("wake false still woke a run")
	}
	if tk, _ := v.s.GetTicket(ctx, id); *tk.Author != "script" {
		t.Fatalf("operator-named author = %s", *tk.Author)
	}
	msg := `{"body":"and Y?","client_message_id":"m1"}`
	code, first := call(t, "POST", u+"/"+id+"/messages", token, msg)
	if code != 201 || first["run_id"] == nil {
		t.Fatalf("message = %d %v", code, first)
	}
	if _, again := call(t, "POST", u+"/"+id+"/messages", token, msg); again["message_id"] != first["message_id"] {
		t.Fatalf("retried message = %v", again)
	}
	page, _ := v.s.TicketThread(ctx, id, nil, 0)
	if len(page.Items) != 3 { // two messages, one run
		t.Fatalf("thread = %d items", len(page.Items))
	}
	if err := v.s.TransitionTicket(ctx, id, store.TicketDone, "kanishk", store.TicketUpdate{}); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/abandon", token, ""); code != 409 {
		t.Fatalf("abandon a done question = %d", code)
	}
}
