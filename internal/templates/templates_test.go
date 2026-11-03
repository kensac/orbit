package templates

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const digest = `{
  "name": "news-digest",
  "description": "Morning headlines on topics you pick.",
  "parameters": [
    {"name": "agent_name", "type": "string", "mode": "required"},
    {"name": "topics", "label": "Topics", "type": "string", "mode": "user_prompt", "description": "What to follow"},
    {"name": "hour", "type": "number", "mode": "optional", "default": 8},
    {"name": "quiet", "type": "boolean", "mode": "optional", "default": false},
    {"name": "tone", "type": "select", "options": ["brief", "detailed"], "mode": "optional", "default": "brief"},
    {"name": "guide", "type": "file", "mode": "optional"}
  ],
  "spec": {
    "name": "{{agent_name}}",
    "description": "Headlines about {{ topics }}.",
    "charter": "Summarize the news about {{topics}} in a {{tone}} way. Never expand {{ hour }}{{ hour }} twice.\n{{guide}}",
    "triggers": [{"schedule": "0 {{hour}} * * *"}],
    "wake_on_message": "{{quiet}}"
  }
}`

func TestATemplateRendersTypedValues(t *testing.T) {
	tp, err := Decode([]byte(digest))
	if err != nil {
		t.Fatal(err)
	}
	// A boolean slot fills a boolean field, so the filled sample decodes as an agent.
	if err := tp.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(tp.Slots(), ","); got != "agent_name,guide,hour,quiet,tone,topics" {
		t.Fatalf("slots = %s", got)
	}
	if m := tp.Missing(map[string]json.RawMessage{"agent_name": json.RawMessage(`"news"`)}, UserPrompt); len(m) != 1 || m[0].Name != "topics" {
		t.Fatalf("missing = %+v", m)
	}
	files := func(id string) (string, error) {
		if id == "11111111-1111-1111-1111-111111111111" {
			return "Write plainly. {{topics}} stays literal.", nil
		}
		return "", errors.New("no such upload")
	}
	out, err := tp.Render(map[string]json.RawMessage{
		"agent_name": json.RawMessage(`"news"`), "topics": json.RawMessage(`"rust and \"go\""`),
		"hour": json.RawMessage(`7`), "quiet": json.RawMessage(`true`),
		"guide": json.RawMessage(`"11111111-1111-1111-1111-111111111111"`)}, files)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["name"] != "news" || got["description"] != `Headlines about rust and "go".` || got["wake_on_message"] != true {
		t.Fatalf("rendered = %s", out)
	}
	if c := got["charter"].(string); c != "Summarize the news about rust and \"go\" in a brief way. Never expand 77 twice.\nWrite plainly. {{topics}} stays literal." {
		t.Fatalf("charter = %q", c)
	}
	if s := got["triggers"].([]any)[0].(map[string]any)["schedule"]; s != "0 7 * * *" {
		t.Fatalf("schedule = %v", s)
	}
	for name, values := range map[string]map[string]json.RawMessage{
		"required missing": {"topics": json.RawMessage(`"x"`)},
		"wrong type":       {"agent_name": json.RawMessage(`"n"`), "topics": json.RawMessage(`"x"`), "hour": json.RawMessage(`"eight"`)},
		"not an option":    {"agent_name": json.RawMessage(`"n"`), "topics": json.RawMessage(`"x"`), "tone": json.RawMessage(`"loud"`)},
		"unknown":          {"agent_name": json.RawMessage(`"n"`), "topics": json.RawMessage(`"x"`), "color": json.RawMessage(`"red"`)},
		"unreadable file":  {"agent_name": json.RawMessage(`"n"`), "topics": json.RawMessage(`"x"`), "guide": json.RawMessage(`"22222222-2222-2222-2222-222222222222"`)},
	} {
		if _, err := tp.Render(values, files); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

func TestValidateRefusesABrokenTemplate(t *testing.T) {
	for name, doc := range map[string]string{
		"name":          `{"name": "News", "description": "d", "parameters": [], "spec": {"charter": "x"}}`,
		"description":   `{"name": "n", "description": "", "parameters": [], "spec": {"charter": "x"}}`,
		"mode":          `{"name": "n", "description": "d", "parameters": [{"name": "a", "type": "string", "mode": "sometimes"}], "spec": {"charter": "{{a}}"}}`,
		"param type":    `{"name": "n", "description": "d", "parameters": [{"name": "a", "type": "color", "mode": "required"}], "spec": {"charter": "{{a}}"}}`,
		"unknown slot":  `{"name": "n", "description": "d", "parameters": [], "spec": {"charter": "{{who}}"}}`,
		"spec field":    `{"name": "n", "description": "d", "parameters": [], "spec": {"charter": "x", "charterr": "y"}}`,
		"spec not obj":  `{"name": "n", "description": "d", "parameters": [], "spec": ["x"]}`,
		"unknown field": `{"name": "n", "description": "d", "parameters": [], "spec": {"charter": "x"}, "extra": 1}`,
	} {
		tp, err := Decode([]byte(doc))
		if err == nil {
			err = tp.Validate()
		}
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
