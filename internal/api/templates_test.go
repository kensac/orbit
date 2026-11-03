package api_test

import (
	"strings"
	"testing"
)

const newsTemplate = `{"template": {
  "name": "news",
  "description": "Morning headlines on topics you pick.",
  "parameters": [
    {"name": "agent", "label": "Agent name", "type": "string", "mode": "required"},
    {"name": "topics", "label": "Topics", "type": "string", "mode": "user_prompt"},
    {"name": "hour", "type": "number", "mode": "optional", "default": 8}
  ],
  "spec": {"name": "{{agent}}", "description": "Headlines about {{topics}}.",
           "charter": "Summarize the news about {{topics}}.", "triggers": [{"schedule": "0 {{hour}} * * *"}]}
}, "note": "first"}`

// TestTemplatesAreAVersionedLibraryAndCreateAgents: the library keeps every version (create, update, unchanged, retire, restore), and creating from a template renders it, dry-runs it, and defines the agent, refusing what is missing with the form that asks for it.
func TestTemplatesAreAVersionedLibraryAndCreateAgents(t *testing.T) {
	v := newEnv(t, nil)
	tok := v.person("kanishk")
	u := v.srv.URL + "/api/v1/templates"
	code, b := call(t, "POST", u, tok, newsTemplate)
	if code != 201 || b["template"].(map[string]any)["version"] != 1.0 || b["template"].(map[string]any)["author"] != "kanishk" {
		t.Fatalf("create = %d %v", code, b)
	}
	if code, _ := call(t, "POST", u, tok, newsTemplate); code != 409 {
		t.Fatalf("a second create = %d", code)
	}
	for name, body := range map[string]string{
		"unknown slot": `{"template": {"name": "x", "description": "d", "parameters": [], "spec": {"charter": "{{who}}"}}}`,
		"bad mode":     `{"template": {"name": "x", "description": "d", "parameters": [{"name": "a", "type": "string", "mode": "maybe"}], "spec": {"charter": "{{a}}"}}}`,
		"spec field":   `{"template": {"name": "x", "description": "d", "parameters": [], "spec": {"chart": "x"}}}`,
		"no template":  `{}`,
	} {
		if code, b := call(t, "POST", u, tok, body); code != 400 {
			t.Errorf("%s: %d %v", name, code, b)
		}
	}
	code, b = call(t, "GET", u, tok, "")
	list := b["templates"].([]any)
	if code != 200 || len(list) != 1 || len(list[0].(map[string]any)["parameters"].([]any)) != 3 || list[0].(map[string]any)["spec"] != nil {
		t.Fatalf("list = %d %v", code, b)
	}
	// The same document again is no version; a change is the next one.
	same := strings.Replace(newsTemplate, `"note": "first"`, `"version": 1`, 1)
	if code, b := call(t, "PUT", u+"/news", tok, same); code != 200 || b["changed"] != false {
		t.Fatalf("unchanged put = %d %v", code, b)
	}
	changed := strings.Replace(same, "Summarize the news", "Summarize, briefly, the news", 1)
	if code, b := call(t, "PUT", u+"/news", tok, changed); code != 200 || b["changed"] != true || b["template"].(map[string]any)["version"] != 2.0 {
		t.Fatalf("put = %d %v", code, b)
	}
	if code, _ := call(t, "PUT", u+"/news", tok, changed); code != 409 {
		t.Fatalf("a put against an old version = %d", code)
	}
	code, b = call(t, "GET", u+"/news", tok, "")
	if code != 200 || !strings.Contains(b["template"].(map[string]any)["spec"].(map[string]any)["charter"].(string), "briefly") || len(b["versions"].([]any)) != 2 {
		t.Fatalf("get = %d %v", code, b)
	}
	if code, b := call(t, "GET", u+"/news/versions/1", tok, ""); code != 200 || strings.Contains(b["template"].(map[string]any)["spec"].(map[string]any)["charter"].(string), "briefly") {
		t.Fatalf("version 1 = %d %v", code, b)
	}

	// Creating: what is missing comes back as the form to ask for it.
	agents := u + "/news/agents"
	code, b = call(t, "POST", agents, tok, `{"parameters": {"agent": "headlines"}}`)
	if code != 400 || !strings.Contains(b["error"].(string), "topics") || len(b["needs"].([]any)) != 1 ||
		b["needs"].([]any)[0].(map[string]any)["label"] != "Topics" {
		t.Fatalf("missing user_prompt = %d %v", code, b)
	}
	if code, b := call(t, "POST", agents, tok, `{"parameters": {"agent": "headlines", "topics": "rust", "hour": "late"}}`); code != 400 || !strings.Contains(b["error"].(string), "hour") {
		t.Fatalf("a wrong type = %d %v", code, b)
	}
	if code, b := call(t, "POST", agents, tok, `{"parameters": {"agent": "headlines", "topics": "rust", "hour": 25}}`); code != 400 {
		t.Fatalf("a rendered agent that does not load (hour 25) = %d %v", code, b)
	}
	code, b = call(t, "POST", agents, tok, `{"parameters": {"agent": "headlines", "topics": "rust"}, "dry_run": true}`)
	if code != 200 || b["valid"] != true || b["definition"] != nil || b["spec"].(map[string]any)["charter"] != "Summarize, briefly, the news about rust." ||
		len(b["next_fires"].([]any)) == 0 || b["template"].(map[string]any)["version"] != 2.0 {
		t.Fatalf("dry run = %d %v", code, b)
	}
	if code, _ := get(t, v.srv.URL+"/api/v1/agents/headlines", tok); code != 404 {
		t.Fatalf("a dry run defined the agent: %d", code)
	}
	code, b = call(t, "POST", agents, tok, `{"version": 1, "name": "morning", "parameters": {"agent": "headlines", "topics": "go"}}`)
	if code != 201 || b["definition"].(map[string]any)["author"] != "kanishk" || b["agent"].(map[string]any)["origin"] != "database" ||
		b["spec"].(map[string]any)["name"] != "morning" || b["spec"].(map[string]any)["charter"] != "Summarize the news about go." {
		t.Fatalf("create = %d %v", code, b)
	}
	if code, _ := call(t, "POST", agents, tok, `{"name": "morning", "parameters": {"agent": "x", "topics": "go"}}`); code != 409 {
		t.Fatalf("a taken name = %d", code)
	}

	// Retired, nothing is created from it; restored, it is the next version.
	if code, b := call(t, "POST", u+"/news/retire", tok, ""); code != 200 || b["template"].(map[string]any)["status"] != "retired" {
		t.Fatalf("retire = %d %v", code, b)
	}
	if code, b := call(t, "POST", agents, tok, `{"parameters": {"agent": "later", "topics": "go"}}`); code != 400 || !strings.Contains(b["error"].(string), "retired") {
		t.Fatalf("create from a retired template = %d %v", code, b)
	}
	if code, b := call(t, "POST", u+"/news/restore", tok, `{"restore": 1}`); code != 200 || b["template"].(map[string]any)["version"] != 4.0 ||
		b["template"].(map[string]any)["restored_from"] != 1.0 {
		t.Fatalf("restore = %d %v", code, b)
	}
	if code, _ := call(t, "GET", u+"/nope", tok, ""); code != 404 {
		t.Fatalf("an unknown template = %d", code)
	}
}
