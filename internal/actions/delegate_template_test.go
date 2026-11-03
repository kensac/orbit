package actions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/forms"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/templates"
)

func TestTemplateDelegateUsesTypedParametersAndConnectorSubset(t *testing.T) {
	tpl := store.Template{Name: "weekly-report", Status: "active", Spec: json.RawMessage(`{"connectors":["calendar","drive"]}`), Parameters: []templates.Parameter{
		{Name: "days", Type: forms.Number, Mode: templates.Required},
		{Name: "include_drafts", Type: forms.Boolean, Mode: templates.Optional},
	}}
	ty := delegateType(nil, []store.Template{tpl}, []string{"calendar", "drive"})
	valid := json.RawMessage(`{"tasks":[{"key":"report","template":"weekly-report","parameters":{"days":7,"include_drafts":false},"connectors":["drive"],"brief":"Prepare the report"}]}`)
	if err := ty.Validate(valid); err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]string{
		`{"tasks":[{"key":"report","template":"weekly-report","parameters":{"days":"seven"},"brief":"Prepare the report"}]}`:                 "number",
		`{"tasks":[{"key":"report","template":"weekly-report","parameters":{"days":7,"unknown":true},"brief":"Prepare the report"}]}`:        "no field named",
		`{"tasks":[{"key":"report","template":"weekly-report","parameters":{"days":7},"connectors":["mail"],"brief":"Prepare the report"}]}`: "connector",
	} {
		if err := ty.Validate(json.RawMessage(input)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate(%s) = %v, want %q", input, err, want)
		}
	}
}

func TestTemplateDelegateRejectsPersonPromptAndAgentTemplateMix(t *testing.T) {
	tpl := store.Template{Name: "private-report", Status: "active", Parameters: []templates.Parameter{
		{Name: "owner", Type: forms.String, Mode: templates.UserPrompt},
	}}
	ty := delegateType(nil, []store.Template{tpl}, nil)
	if err := ty.Validate(json.RawMessage(`{"tasks":[{"key":"report","template":"private-report","brief":"Prepare the report"}]}`)); err == nil || !strings.Contains(err.Error(), "not one this agent may delegate to") {
		t.Fatalf("person-prompt template was offered: %v", err)
	}
	task, err := DecodeDelegate(json.RawMessage(`{"tasks":[{"key":"report","agent":"reader","template":"weekly-report","brief":"Prepare the report"}]}`), []string{"reader"}, []string{"weekly-report"})
	if err == nil || len(task.Tasks) != 1 {
		t.Fatalf("agent and template together accepted: %#v, %v", task, err)
	}
}
