package api_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
)

func TestDecidingAnApprovalOverTheAPI(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	base := v.srv.URL + "/api/v1"
	a := v.e.Current().Config.Agents["a"]
	run, err := v.s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: a.SHA, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	var conv string
	if err := v.s.Pool.QueryRow(ctx, `SELECT conversation_id::text FROM runs WHERE id = $1`, run).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	mk := func(id, kind string, grant bool) string {
		ap, err := v.s.CreateApproval(ctx, store.NewApproval{RunID: run, Attempt: 1, ConversationID: conv, Agent: "a", RequestID: id, Kind: kind,
			Tool: "Bash", Level: "ask", Input: json.RawMessage(`{"command":"ls"}`), MatchedRule: json.RawMessage(`{"source":"cliArg","tool_name":"Bash"}`),
			ConversationGrant: grant, ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		return ap.ID
	}
	tool, plan := mk("q1", "tool", true), mk("q2", "plan", false)

	code, r := call(t, "GET", base+"/conversations/"+conv+"/approvals?state=pending", kanishk, "")
	if code != 200 || len(r["approvals"].([]any)) != 2 {
		t.Fatalf("pending = %d %v", code, r)
	}
	code, r = call(t, "GET", base+"/approvals/"+tool, kanishk, "")
	ap := r["approval"].(map[string]any)
	if code != 200 || ap["state"] != "pending" || ap["matched_rule"].(map[string]any)["tool_name"] != "Bash" || ap["scope"] != nil {
		t.Fatalf("get = %d %v", code, r)
	}
	for body, want := range map[string]int{
		`{}`:                               400,
		`{"allow":true,"scope":"forever"}`: 400,
		`{"allow":true,"color":"blue"}`:    400,
	} {
		if code, r := call(t, "POST", base+"/approvals/"+tool, kanishk, body); code != want {
			t.Errorf("%s = %d %v", body, code, r)
		}
	}
	if code, r := call(t, "POST", base+"/approvals/"+plan, kanishk, `{"allow":true,"scope":"conversation"}`); code != 400 {
		t.Fatalf("a plan for the conversation = %d %v", code, r)
	}
	code, r = call(t, "POST", base+"/approvals/"+tool, kanishk, `{"allow":true,"scope":"conversation","by":"mallory"}`)
	ap = r["approval"].(map[string]any)
	if code != 200 || ap["state"] != "allowed" || ap["scope"] != "conversation" || ap["decided_by"] != "kanishk" {
		t.Fatalf("allow = %d %v", code, r)
	}
	if code, _ := call(t, "POST", base+"/approvals/"+tool, kanishk, `{"allow":false}`); code != 409 {
		t.Fatalf("a second decision = %d", code)
	}
	code, r = call(t, "POST", base+"/approvals/"+plan, kanishk, `{"allow":false,"message":"plan the tests too"}`)
	if ap := r["approval"].(map[string]any); code != 200 || ap["state"] != "denied" || ap["scope"] != "once" || ap["message"] != "plan the tests too" {
		t.Fatalf("deny = %d %v", code, r)
	}
	if code, _ := call(t, "GET", base+"/conversations/"+conv+"/approvals?state=bogus", kanishk, ""); code != 400 {
		t.Fatalf("bad state = %d", code)
	}
	if code, _ := call(t, "POST", base+"/approvals/00000000-0000-0000-0000-000000000000", kanishk, `{"allow":true}`); code != 404 {
		t.Fatalf("unknown = %d", code)
	}
}
