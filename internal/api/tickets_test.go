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
	base := v.srv.URL + "/api/v1"
	u := base + "/tickets"
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
	if code != 201 || r["run_id"] == nil || r["conversation_id"] == nil {
		t.Fatalf("create = %d %v", code, r)
	}
	id, conv := r["ticket_id"].(string), r["conversation_id"].(string)
	if code, again := call(t, "POST", u, kanishk, create); code != 201 || again["ticket_id"] != id || again["run_id"] != r["run_id"] {
		t.Fatalf("retry = %d %v", code, again)
	}
	if tk, _ := v.s.GetTicket(ctx, id); *tk.Author != "kanishk" {
		t.Fatalf("author = %s, want the token's person", *tk.Author)
	}
	// The ticket's conversation is listed with its ticket_id, and its
	// thread is the conversation's.
	_, r = call(t, "GET", base+"/agents/a/conversations", kanishk, "")
	if first := r["conversations"].([]any)[0].(map[string]any); first["id"] != conv || first["ticket_id"] != id {
		t.Fatalf("conversations = %v", r)
	}
	if code, r := call(t, "GET", base+"/conversations/"+conv+"/thread", kanishk, ""); code != 200 || len(r["items"].([]any)) != 2 {
		t.Fatalf("ticket thread = %d %v", code, r)
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
	if code != 200 || ticket["status"] != "approved" || ticket["conversation_id"] != conv ||
		len(r["plans"].([]any)) != 1 || len(r["events"].([]any)) != 3 {
		t.Fatalf("get = %d %v", code, r)
	}
	if by := r["events"].([]any)[2].(map[string]any)["by"]; by != "kanishk" {
		t.Fatalf("approved by %v, want the token's person", by)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/reopen", kanishk, ""); code != 409 {
		t.Fatalf("reopen an approved ticket = %d", code)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/abandon", kanishk, `{"reason":"later"}`); code != 200 {
		t.Fatalf("abandon = %d", code)
	}
	if code, _ := call(t, "POST", base+"/conversations/"+conv+"/messages", kanishk, `{"body":"more"}`); code != 409 {
		t.Fatalf("message to an abandoned ticket = %d", code)
	}
	if code, _ := call(t, "GET", u+"/00000000-0000-0000-0000-000000000000", kanishk, ""); code != 404 {
		t.Fatalf("unknown ticket = %d", code)
	}
	if code, r := call(t, "GET", u+"?status=abandoned", kanishk, ""); code != 200 || len(r["tickets"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, r)
	}
}

func TestTicketsAndTheOperatorToken(t *testing.T) {
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
	if err := v.s.TransitionTicket(ctx, id, store.TicketDone, "kanishk", store.TicketUpdate{}); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/abandon", token, ""); code != 409 {
		t.Fatalf("abandon a done question = %d", code)
	}
}
