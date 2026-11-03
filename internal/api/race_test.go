package api_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
)

// A race over the API: filed with lanes and variants, refused while the
// subscription is nearly spent, compared in the ticket review, and picked
// by a person.
func TestARaceOverTheAPI(t *testing.T) {
	v := newEnv(t, nil)
	kanishk := v.person("kanishk")
	u := v.srv.URL + "/api/v1/tickets"
	for body, want := range map[string]string{
		`{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/orbit","race":4}`:                                "race is 2 to 3",
		`{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/orbit","race":2,"variants":[{},{},{}]}`:          "3 variants for 2 lanes",
		`{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/orbit","race":2,"variants":[{"effort":"huge"}]}`: "effort must be one of",
		`{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/orbit","race":2,"variants":[{"model":"x; rm"}]}`: "not a model name",
		`{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/orbit","race":2,"variants":[{"temperature":1}]}`: "unknown field",
		`{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/orbit","variants":[{"model":"opus"}]}`:           "variants need a race",
	} {
		if code, r := call(t, "POST", u, kanishk, body); code != 400 || !contains(r["error"], want) {
			t.Errorf("%s = %d %v, want %q", body, code, r, want)
		}
	}
	// Nearly spent: a race is refused, a plain request is not.
	resets := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := v.s.RecordUsageSample(ctx, "test", 200, json.RawMessage(`{"five_hour":{"utilization":93,"resets_at":"`+resets+`"}}`), nil); err != nil {
		t.Fatal(err)
	}
	race := `{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/orbit","wake":false,"race":2,"variants":[{"model":"opus","effort":"high"}]}`
	if code, r := call(t, "POST", u, kanishk, race); code != 409 || !contains(r["error"], "five_hour window is at 93%") {
		t.Fatalf("race while nearly spent = %d %v", code, r)
	}
	if code, _ := call(t, "POST", u, kanishk, `{"kind":"request","title":"t","body":"b","agent":"a","repo":"kensac/orbit","wake":false}`); code != 201 {
		t.Fatalf("a plain request while nearly spent = %d", code)
	}
	if err := v.s.RecordUsageSample(ctx, "test", 200, json.RawMessage(`{"five_hour":{"utilization":20,"resets_at":"`+resets+`"}}`), nil); err != nil {
		t.Fatal(err)
	}
	code, r := call(t, "POST", u, kanishk, race)
	if code != 201 {
		t.Fatalf("race = %d %v", code, r)
	}
	id := r["ticket_id"].(string)
	_, _ = v.s.ProposePlan(ctx, id, "", "plan")
	_, _ = v.s.ApprovePlan(ctx, id, 1, "kanishk", "")
	a := v.e.Current().Config.Agents["a"]
	runs, err := v.s.StartRace(ctx, store.StartBuild{TicketID: id, Agent: "a", SpecSHA: a.SHA, Purpose: store.BuildPurposeBuild})
	if err != nil || len(runs) != 2 {
		t.Fatalf("start race = %v %v", runs, err)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/pick", kanishk, `{"lane":"a"}`); code != 409 {
		t.Fatalf("a pick while building = %d", code)
	}
	for i := range 2 {
		c, _ := v.s.ClaimRun(ctx, "w")
		b, _ := v.s.BuildOfRun(ctx, c.ID)
		lane := *b.Lane
		pr := map[string]int{"a": 7, "b": 8}[lane]
		lines := map[string]int{"a": 40, "b": 12}[lane]
		if err := v.s.FinishBuild(ctx, c.ID, store.RunResult{Attempt: c.Attempt, Summary: "built"},
			store.TicketUpdate{Branch: "orbit/x-" + lane, PRNumber: &pr, HeadSHA: "11111111111111111111111111111111111111" + lane + lane,
				Diff: &store.BuildDiff{Added: lines, Files: []string{"x.go", "x_test.go"}}}); err != nil {
			t.Fatal(i, err)
		}
	}
	code, r = call(t, "GET", u+"/"+id, kanishk, "")
	rc, _ := r["race"].(map[string]any)
	if code != 200 || rc == nil || rc["recommended"] != "b" || r["ticket"].(map[string]any)["race"] != float64(2) {
		t.Fatalf("get = %d %v", code, r)
	}
	lanes := rc["lanes"].([]any)
	first := lanes[0].(map[string]any)
	if len(lanes) != 2 || first["lane"] != "b" || first["rank"] != float64(1) || first["lines_changed"] != float64(12) ||
		first["tests_touched"] != float64(1) || first["delivered"] != true {
		t.Fatalf("lanes = %v", lanes)
	}
	if vr := lanes[1].(map[string]any)["variant"].(map[string]any); vr["model"] != "opus" || vr["effort"] != "high" {
		t.Fatalf("lane a's variant = %v", vr)
	}
	if code, r := call(t, "POST", u+"/"+id+"/pick", kanishk, `{"lane":"d"}`); code != 400 || !contains(r["error"], "lane must be one of") {
		t.Fatalf("pick lane d = %d %v", code, r)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/pick", kanishk, ``); code != 400 {
		t.Fatalf("a pick with no body = %d", code)
	}
	pick := `{"lane":"a","by":"mallory","client_request_id":"p1"}`
	code, r = call(t, "POST", u+"/"+id+"/pick", kanishk, pick)
	if code != 200 || r["ok"] != true || r["action_id"] == nil {
		t.Fatalf("pick = %d %v", code, r)
	}
	if code, again := call(t, "POST", u+"/"+id+"/pick", kanishk, pick); code != 200 || again["action_id"] != r["action_id"] {
		t.Fatalf("pick retried = %d %v", code, again)
	}
	if code, _ := call(t, "POST", u+"/"+id+"/pick", kanishk, `{"lane":"b"}`); code != 409 {
		t.Fatalf("a second pick = %d", code)
	}
	act, err := v.s.GetAction(ctx, r["action_id"].(string))
	if err != nil || act.Type != store.PickType || act.Status != "executed" || act.Rule != "approve" || *act.DecidedBy != "kanishk" {
		t.Fatalf("pick action = %+v %v", act, err)
	}
	_, r = call(t, "GET", u+"/"+id, kanishk, "")
	tk := r["ticket"].(map[string]any)
	if tk["picked_lane"] != "a" || tk["pr_number"] != float64(7) || tk["branch"] != "orbit/x-a" || r["race"].(map[string]any)["picked"] != "a" {
		t.Fatalf("picked ticket = %v", r)
	}
}
