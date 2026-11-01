package api_test

import (
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
)

func TestTicketLifecycleOverTheAPI(t *testing.T) {
	srv, s, _ := server(t)
	u := srv.URL + "/api/v1/tickets"
	for body, want := range map[string]string{
		`{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/other"}`:     "not in github.repositories",
		`{"kind":"request","title":"t","body":"b","agent":"ghost","repo":"kensac/orbit"}`: "not configured",
		`{"kind":"question","title":"t","body":"b","agent":"a","repo":"kensac/orbit"}`:    "does not name a repository",
		`{"kind":"bug","title":"t","body":"b","agent":"a"}`:                               "kind must be",
		`{"kind":"question","title":" ","body":"b","agent":"a"}`:                          "title and body",
	} {
		if code, v := call(t, "POST", u, token, body); code != 400 || !contains(v["error"], want) {
			t.Errorf("%s = %d %v, want %q", body, code, v, want)
		}
	}
	code, v := call(t, "POST", u, token, `{"kind":"request","title":"Add a flag","body":"please","agent":"a","repo":"kensac/orbit","author":"kanishk"}`)
	if code != 201 || v["run_id"] == nil {
		t.Fatalf("create = %d %v, want a run woken on the ticket", code, v)
	}
	id := v["ticket_id"].(string)
	runs, _ := s.TicketRuns(ctx, id)
	if len(runs) != 1 || runs[0].ID != v["run_id"] {
		t.Fatalf("ticket runs = %+v", runs)
	}

	if code, _ := call(t, "POST", u+"/"+id+"/plans/1/approve", token, `{"by":"kanishk"}`); code != 409 {
		t.Fatalf("approve with no plan = %d", code)
	}
	if _, err := s.ProposePlan(ctx, id, "", "the plan"); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/plans/x/approve", token, ""); code != 400 {
		t.Fatalf("bad version = %d", code)
	}
	if code, v := call(t, "POST", u+"/"+id+"/plans/1/approve", token, `{"by":"kanishk"}`); code != 200 || v["ok"] != true {
		t.Fatalf("approve = %d %v", code, v)
	}
	code, v = call(t, "GET", u+"/"+id, token, "")
	tk := v["ticket"].(map[string]any)
	if code != 200 || tk["status"] != "approved" || len(v["plans"].([]any)) != 1 || len(v["events"].([]any)) != 3 ||
		len(v["messages"].([]any)) != 1 || len(v["runs"].([]any)) != 1 {
		t.Fatalf("get = %d %v", code, v)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/reopen", token, ""); code != 409 {
		t.Fatalf("reopen an approved ticket = %d", code)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/abandon", token, `{"by":"kanishk","reason":"later"}`); code != 200 {
		t.Fatalf("abandon = %d", code)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/messages", token, `{"body":"more"}`); code != 409 {
		t.Fatalf("message to an abandoned ticket = %d", code)
	}
	if code, _ := call(t, "GET", u+"/00000000-0000-0000-0000-000000000000", token, ""); code != 404 {
		t.Fatalf("unknown ticket = %d", code)
	}
	code, v = call(t, "GET", u+"?status=abandoned", token, "")
	if code != 200 || len(v["tickets"].([]any)) != 1 {
		t.Fatalf("list = %d %v", code, v)
	}
}

func TestQuestionTicketMessagesWakeItsThread(t *testing.T) {
	srv, s, _ := server(t)
	u := srv.URL + "/api/v1/tickets"
	_, v := call(t, "POST", u, token, `{"kind":"question","title":"Why?","body":"why is X","agent":"a","wake":false}`)
	id := v["ticket_id"].(string)
	if v["run_id"] != nil {
		t.Fatal("wake false still woke a run")
	}
	code, v := call(t, "POST", u+"/"+id+"/messages", token, `{"body":"and Y?","author":"kanishk"}`)
	if code != 201 || v["run_id"] == nil {
		t.Fatalf("message = %d %v", code, v)
	}
	if err := s.TransitionTicket(ctx, id, store.TicketDone, "kanishk", store.TicketUpdate{}); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/abandon", token, ""); code != 409 {
		t.Fatalf("abandon a done question = %d", code)
	}
}

func contains(v any, sub string) bool {
	s, _ := v.(string)
	return strings.Contains(s, sub)
}
