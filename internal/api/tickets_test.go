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

func TestRerunningChecksOverTheAPI(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	u := v.srv.URL + "/api/v1/tickets"
	_, r := call(t, "POST", u, kanishk, `{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/orbit","wake":false}`)
	id := r["ticket_id"].(string)
	if code, _ := call(t, "POST", u+"/"+id+"/checks/rerun", kanishk, ""); code != 409 {
		t.Fatalf("rerun with no PR = %d", code)
	}
	if _, err := v.s.ProposePlan(ctx, id, "", "plan"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.s.ApprovePlan(ctx, id, 1, "kanishk", ""); err != nil {
		t.Fatal(err)
	}
	a := v.e.Current().Config.Agents["a"]
	run, err := v.s.StartBuild(ctx, store.StartBuild{TicketID: id, Agent: "a", SpecSHA: a.SHA, Purpose: store.BuildPurposeBuild})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := v.s.ClaimRun(ctx, "w")
	pr := 7
	if c == nil || c.ID != run {
		t.Fatalf("claim = %+v", c)
	}
	if err := v.s.FinishBuild(ctx, run, store.RunResult{Attempt: c.Attempt, Summary: "built"},
		store.TicketUpdate{Branch: "orbit/x", PRNumber: &pr, HeadSHA: "1111111111111111111111111111111111111111"}); err != nil {
		t.Fatal(err)
	}
	code, r := call(t, "POST", u+"/"+id+"/checks/rerun", kanishk, `{"by":"mallory"}`)
	rr, _ := r["rerun"].(map[string]any)
	if code != 202 || rr["requested_by"] != "kanishk" || rr["done_at"] != nil {
		t.Fatalf("rerun = %d %v", code, r)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/checks/rerun", kanishk, ""); code != 409 {
		t.Fatalf("a second rerun while one waits = %d", code)
	}
	if _, r := call(t, "GET", u+"/"+id, kanishk, ""); len(r["reruns"].([]any)) != 1 {
		t.Fatalf("get = %v", r)
	}
}

func TestReviewCommentsOverTheAPI(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	u := v.srv.URL + "/api/v1/tickets"
	_, r := call(t, "POST", u, kanishk, `{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/orbit","wake":false}`)
	id := r["ticket_id"].(string)
	if code, _ := call(t, "POST", u+"/"+id+"/comments", kanishk, `{"path":"a.go","start_line":1,"end_line":1,"body":"x"}`); code != 409 {
		t.Fatalf("a comment with no PR = %d", code)
	}
	_, _ = v.s.ProposePlan(ctx, id, "", "plan")
	_, _ = v.s.ApprovePlan(ctx, id, 1, "kanishk", "")
	a := v.e.Current().Config.Agents["a"]
	run, _ := v.s.StartBuild(ctx, store.StartBuild{TicketID: id, Agent: "a", SpecSHA: a.SHA, Purpose: store.BuildPurposeBuild})
	c, _ := v.s.ClaimRun(ctx, "w")
	pr := 7
	if err := v.s.FinishBuild(ctx, run, store.RunResult{Attempt: c.Attempt, Summary: "built"},
		store.TicketUpdate{Branch: "orbit/x", PRNumber: &pr, HeadSHA: "1111111111111111111111111111111111111111"}); err != nil {
		t.Fatal(err)
	}
	if code, r := call(t, "POST", u+"/"+id+"/comments", kanishk, `{"path":"../x","start_line":1,"end_line":1,"body":"x"}`); code != 400 || !contains(r["error"], "clean path") {
		t.Fatalf("bad path = %d %v", code, r)
	}
	code, r := call(t, "POST", u+"/"+id+"/comments", kanishk, `{"path":"a.go","start_line":2,"end_line":4,"body":"tighten this","author":"mallory"}`)
	cm, _ := r["comment"].(map[string]any)
	if code != 201 || cm["status"] != "draft" || cm["author"] != "kanishk" {
		t.Fatalf("add = %d %v", code, r)
	}
	cid := cm["id"].(string)
	code, r = call(t, "POST", u+"/"+id+"/comments/"+cid+"/edit", kanishk, `{"start_line":2,"end_line":5,"body":"tighten this, and test it"}`)
	if v2, _ := r["comment"].(map[string]any); code != 201 || v2["supersedes"] != cid || v2["path"] != "a.go" {
		t.Fatalf("edit = %d %v", code, r)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/comments/"+cid+"/withdraw", kanishk, ""); code != 409 {
		t.Fatalf("withdraw a superseded version = %d", code)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/comments/not-a-uuid/withdraw", kanishk, ""); code != 400 {
		t.Fatalf("withdraw a bad id = %d", code)
	}
	code, r = call(t, "POST", u+"/"+id+"/review", kanishk, `{"body":"One thing.","client_request_id":"rv"}`)
	if code != 201 || r["comments"] != float64(1) || r["message_id"] == nil {
		t.Fatalf("submit = %d %v", code, r)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/review", kanishk, `{}`); code != 400 {
		t.Fatalf("a review with nothing to send = %d", code)
	}
	_, r = call(t, "GET", u+"/"+id+"/comments", kanishk, "")
	cs := r["comments"].([]any)
	if len(cs) != 2 || cs[0].(map[string]any)["status"] != "superseded" || cs[1].(map[string]any)["status"] != "submitted" {
		t.Fatalf("comments = %v", cs)
	}
}
