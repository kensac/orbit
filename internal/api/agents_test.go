package api_test

import (
	"testing"
)

func TestDefiningAnAgentThroughTheAPI(t *testing.T) {
	v := newEnv(t, nil)
	who := v.person("kanishk")
	u := v.srv.URL + "/api/v1/agents"

	// A dry run validates with the whole configuration and stores nothing.
	code, body := call(t, "POST", u, who, `{"dry_run": true, "spec": {"name": "news", "charter": "Read the news.",
		"triggers": [{"schedule": "0 8 * * *"}]}}`)
	if code != 200 || body["valid"] != true || body["definition"] != nil {
		t.Fatalf("dry run = %d %v", code, body)
	}
	if fires, _ := body["next_fires"].([]any); len(fires) != 1 {
		t.Fatalf("a dry run reports the next fires: %v", body["next_fires"])
	}
	if code, body := call(t, "POST", u, who, `{"spec": {"name": "bad", "charter": "x", "runner": "nope"}}`); code != 400 {
		t.Fatalf("an invalid spec = %d %v", code, body)
	}
	if code, _ := call(t, "POST", u, who, `{"spec": {"name": "a", "charter": "x"}}`); code != 409 {
		t.Fatalf("a file agent's name = %d", code)
	}
	if code, _ := call(t, "POST", u, who, `{"spec": {"name": "x", "charter": "x", "charterz": "y"}}`); code != 400 {
		t.Fatalf("an unknown field = %d", code)
	}

	code, body = call(t, "POST", u, who, `{"spec": {"name": "news", "charter": "Read the news.", "wake_on_message": true}}`)
	if code != 201 {
		t.Fatalf("create = %d %v", code, body)
	}
	def := body["definition"].(map[string]any)
	if def["version"].(float64) != 1 || def["author"] != "kanishk" || def["change"] != "create" {
		t.Fatalf("definition = %v", def)
	}
	if row := body["agent"].(map[string]any); row["origin"] != "database" || row["enabled"] != true {
		t.Fatalf("agent = %v", row)
	}
	if v.e.Current().Config.Agents["news"] == nil {
		t.Fatal("the new agent is in force at once")
	}
	if code, _ := call(t, "POST", u, who, `{"spec": {"name": "news", "charter": "again"}}`); code != 409 {
		t.Fatalf("creating it twice = %d", code)
	}

	code, body = call(t, "PUT", u+"/news", who, `{"spec": {"charter": "Read the news, briefly."}}`)
	if code != 200 || body["definition"].(map[string]any)["version"].(float64) != 2 {
		t.Fatalf("update = %d %v", code, body)
	}
	if got := v.e.Current().Config.Agents["news"].Charter; got != "Read the news, briefly." {
		t.Fatalf("charter = %q", got)
	}
	if def := body["definition"].(map[string]any); def["edited_by"] != "kanishk" || def["run_id"] != nil {
		t.Fatalf("a person's change: %v", def)
	}
	// A revert is a new version with the earlier spec.
	code, body = call(t, "POST", u+"/news/definitions/1/revert", who, "")
	if code != 200 || body["definition"].(map[string]any)["version"].(float64) != 3 || body["definition"].(map[string]any)["reverts"].(float64) != 1 {
		t.Fatalf("revert = %d %v", code, body)
	}
	if got := v.e.Current().Config.Agents["news"].Charter; got != "Read the news." {
		t.Fatalf("reverted charter = %q", got)
	}
	if code, _ := call(t, "PUT", u+"/a", who, `{"spec": {"charter": "y"}}`); code != 409 {
		t.Fatalf("updating a file agent = %d", code)
	}

	code, body = call(t, "POST", u+"/news/disable", who, "")
	if code != 200 || v.e.Current().Config.Agents["news"].IsEnabled() {
		t.Fatalf("disable = %d %v", code, body)
	}
	if code, _ := call(t, "POST", u+"/news/enable", who, ""); code != 200 || !v.e.Current().Config.Agents["news"].IsEnabled() {
		t.Fatalf("enable = %d", code)
	}
	if code, _ := call(t, "POST", u+"/news/archive", who, ""); code != 200 || v.e.Current().Config.Agents["news"] != nil {
		t.Fatalf("archive = %d", code)
	}
	code, body = call(t, "GET", u+"/news/definitions", who, "")
	if vs, _ := body["versions"].([]any); code != 200 || len(vs) != 6 {
		t.Fatalf("history = %d %v: every version is kept", code, body)
	}
	if code, _ := call(t, "PUT", u+"/news", who, `{"spec": {"charter": "z"}}`); code != 404 {
		t.Fatalf("updating an archived agent = %d", code)
	}
	// The name can be defined again after archiving: a new version.
	if code, body := call(t, "POST", u, who, `{"spec": {"name": "news", "charter": "Back."}}`); code != 201 ||
		body["definition"].(map[string]any)["version"].(float64) != 7 {
		t.Fatalf("re-create = %d %v", code, body)
	}
}

func TestTheBuiltinAgentCanBeReplaced(t *testing.T) {
	v := newEnv(t, nil)
	if err := v.e.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if a := v.e.Current().Config.Agents["orbit"]; a == nil || a.Origin != "builtin" {
		t.Fatalf("orbit = %+v", a)
	}
	code, body := call(t, "PUT", v.srv.URL+"/api/v1/agents/orbit", v.person("kanishk"), `{"spec": {"charter": "Mine now.", "wake_on_message": true}}`)
	if code != 200 {
		t.Fatalf("replace = %d %v", code, body)
	}
	if a := v.e.Current().Config.Agents["orbit"]; a.Origin != "database" || a.Charter != "Mine now." {
		t.Fatalf("orbit = %+v", a)
	}
}
