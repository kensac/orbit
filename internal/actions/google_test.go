package actions_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/mail"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/spec"
)

func googleDeployment() spec.Deployment {
	return spec.Deployment{GoogleActions: &spec.GoogleActions{
		GmailDraft:     &spec.GoogleActionBinding{Connector: "gws", Tool: "create_draft"},
		CalendarCreate: &spec.GoogleCalendarBinding{Connector: "gws", Tool: "create_event", CalendarID: "team@example.com"},
	}}
}

func TestGoogleActionsAreOfferedOnlyWhenConfiguredAndNamed(t *testing.T) {
	a := &spec.Agent{Actions: spec.ActionSet{actions.GmailDraftType: spec.RuleApprove, actions.CalendarCreateType: spec.RuleAuto}}
	has := func(d spec.Deployment, name string) bool {
		for _, ty := range actions.Catalog(a, d) {
			if ty.Name == name {
				return true
			}
		}
		return false
	}
	if !has(googleDeployment(), actions.GmailDraftType) || !has(googleDeployment(), actions.CalendarCreateType) {
		t.Fatal("configured and named actions were not offered")
	}
	if has(spec.Deployment{}, actions.GmailDraftType) || has(spec.Deployment{}, actions.CalendarCreateType) {
		t.Fatal("unconfigured Google actions were offered")
	}
	if has(googleDeployment(), "google:send") {
		t.Fatal("unknown Google action was offered")
	}
	if got := actions.Catalog(&spec.Agent{}, googleDeployment()); len(got) != 0 {
		t.Fatalf("unnamed Google actions were offered: %+v", got)
	}
}

func TestGmailDraftBuildsBoundedDraftRequest(t *testing.T) {
	d := googleDeployment()
	a := &spec.Agent{Actions: spec.ActionSet{actions.GmailDraftType: spec.RuleApprove}}
	var typ actions.Type
	for _, candidate := range actions.Catalog(a, d) {
		if candidate.Name == actions.GmailDraftType {
			typ = candidate
		}
	}
	params := json.RawMessage(`{"to":["Alice <alice@example.com>"],"cc":["bob@example.com"],"subject":"Résumé","body":"Hello, world"}`)
	if err := typ.Validate(params); err != nil {
		t.Fatal(err)
	}
	var called actions.GoogleCall
	ex := &actions.Executor{Deployment: d, Google: func(_ context.Context, _ actions.Call, g actions.GoogleCall) (json.RawMessage, error) {
		called = g
		return json.RawMessage(`{"draftId":"d1"}`), nil
	}}
	result, err := ex.Execute(context.Background(), actions.Call{Agent: "a", Type: actions.GmailDraftType, Params: params})
	if err != nil || string(result) != `{"draftId":"d1"}` {
		t.Fatalf("execute = %s, %v", result, err)
	}
	if called.Connector != "gws" || called.Tool != "create_draft" {
		t.Fatalf("destination = %+v", called)
	}
	var args struct {
		UserID  string `json:"userId"`
		Message struct {
			Raw string `json:"raw"`
		} `json:"message"`
	}
	if err := json.Unmarshal(called.Arguments, &args); err != nil || args.UserID != "me" || args.Message.Raw == "" {
		t.Fatalf("arguments = %s, %v", called.Arguments, err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(args.Message.Raw)
	if err != nil {
		t.Fatal(err)
	}
	m, err := mail.ReadMessage(strings.NewReader(string(decoded)))
	if err != nil {
		t.Fatal(err)
	}
	if m.Header.Get("To") != `"Alice" <alice@example.com>` || !strings.Contains(m.Header.Get("Cc"), "bob@example.com") || !strings.Contains(m.Header.Get("Subject"), "=?UTF-8?") {
		t.Fatalf("headers = %v", m.Header)
	}
	bodyText, err := io.ReadAll(m.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.ReplaceAll(string(bodyText), "\r\n", "")))
	if err != nil || string(body) != "Hello, world" {
		t.Fatalf("body = %q, %v", body, err)
	}
}

func TestCalendarCreateUsesConfiguredCalendarAndValidEvent(t *testing.T) {
	d := googleDeployment()
	a := &spec.Agent{Actions: spec.ActionSet{actions.CalendarCreateType: spec.RuleApprove}}
	var typ actions.Type
	for _, candidate := range actions.Catalog(a, d) {
		if candidate.Name == actions.CalendarCreateType {
			typ = candidate
		}
	}
	params := json.RawMessage(`{"summary":"Planning","description":"Quarterly review","location":"Room 2","start":"2026-11-05T09:00:00-05:00","end":"2026-11-05T10:00:00-05:00","time_zone":"America/New_York","attendees":["alice@example.com"]}`)
	if err := typ.Validate(params); err != nil {
		t.Fatal(err)
	}
	var called actions.GoogleCall
	ex := &actions.Executor{Deployment: d, Google: func(_ context.Context, _ actions.Call, g actions.GoogleCall) (json.RawMessage, error) {
		called = g
		return json.RawMessage(`{"id":"event-1"}`), nil
	}}
	if _, err := ex.Execute(context.Background(), actions.Call{Agent: "a", Type: actions.CalendarCreateType, Params: params}); err != nil {
		t.Fatal(err)
	}
	var args struct {
		CalendarID string `json:"calendarId"`
		Event      struct {
			Summary   string            `json:"summary"`
			Start     map[string]string `json:"start"`
			End       map[string]string `json:"end"`
			Attendees []struct {
				Email string `json:"email"`
			} `json:"attendees"`
		} `json:"event"`
	}
	if err := json.Unmarshal(called.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if called.Connector != "gws" || called.Tool != "create_event" || args.CalendarID != "team@example.com" ||
		args.Event.Summary != "Planning" || args.Event.Start["timeZone"] != "America/New_York" ||
		len(args.Event.Attendees) != 1 || args.Event.Attendees[0].Email != "alice@example.com" {
		t.Fatalf("call=%+v arguments=%s", called, called.Arguments)
	}
}

func TestGoogleActionsRejectInvalidFieldsAndUnconfiguredExecution(t *testing.T) {
	d := googleDeployment()
	tests := []struct {
		typ, params, want string
	}{
		{actions.GmailDraftType, `{"to":["a@example.com\r\nBcc: victim@example.com"],"subject":"s","body":"b"}`, "invalid email"},
		{actions.GmailDraftType, `{"to":["a@example.com"],"subject":"s\r\nBcc: x","body":"b"}`, "without line breaks"},
		{actions.GmailDraftType, `{"to":["a@example.com"],"subject":"s","body":"b","send":true}`, "unknown field"},
		{actions.CalendarCreateType, `{"summary":"x","start":"2026-11-05T10:00:00Z","end":"2026-11-05T09:00:00Z"}`, "after start"},
		{actions.CalendarCreateType, `{"summary":"x","start":"tomorrow","end":"2026-11-05T09:00:00Z"}`, "RFC 3339"},
		{actions.CalendarCreateType, `{"summary":"x","start":"2026-11-05T08:00:00Z","end":"2026-11-05T09:00:00Z","time_zone":"Not/AZone"}`, "IANA time zone"},
	}
	for _, tc := range tests {
		t.Run(tc.typ+tc.want, func(t *testing.T) {
			ex := &actions.Executor{Deployment: d}
			_, err := ex.Execute(context.Background(), actions.Call{Agent: "a", Type: tc.typ, Params: json.RawMessage(tc.params)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
