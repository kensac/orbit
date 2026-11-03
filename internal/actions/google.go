package actions

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

const (
	GmailDraftType     = "gmail:draft"
	CalendarCreateType = "calendar:create"
	maxGoogleText      = 64 << 10
	maxGoogleAttendees = 50
)

// GoogleCall is a fixed connector destination and a code-built Google API
// request. The agent supplies only the typed fields, never a connector, tool,
// calendar id or URL.
type GoogleCall struct {
	Connector string
	Tool      string
	Arguments json.RawMessage
}

type gmailDraftParams struct {
	To      []string `json:"to"`
	CC      []string `json:"cc,omitempty"`
	BCC     []string `json:"bcc,omitempty"`
	Subject string   `json:"subject"`
	Body    string   `json:"body"`
}

type calendarCreateParams struct {
	Summary     string   `json:"summary"`
	Description string   `json:"description,omitempty"`
	Location    string   `json:"location,omitempty"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	TimeZone    string   `json:"time_zone,omitempty"`
	Attendees   []string `json:"attendees,omitempty"`
}

func googleTypes(a *spec.Agent, d spec.Deployment) []Type {
	var out []Type
	if d.GoogleActions == nil {
		return out
	}
	if d.GoogleActions.GmailDraft != nil && a.RuleFor(GmailDraftType) != spec.RuleDeny {
		out = append(out, Type{Name: GmailDraftType,
			Description: "Create a Gmail draft in the configured account. This saves a draft and never sends it.",
			ParamsSchema: map[string]any{
				"type": "object", "properties": map[string]any{
					"to":      map[string]any{"type": "array", "items": emailSchema(), "minItems": 1, "maxItems": 20},
					"cc":      map[string]any{"type": "array", "items": emailSchema(), "maxItems": 20},
					"bcc":     map[string]any{"type": "array", "items": emailSchema(), "maxItems": 20},
					"subject": map[string]any{"type": "string", "minLength": 1, "maxLength": 255},
					"body":    map[string]any{"type": "string", "minLength": 1, "maxLength": maxGoogleText},
				}, "required": []string{"to", "subject", "body"}, "additionalProperties": false,
			}, Validate: func(p json.RawMessage) error { _, err := googleCall(d, GmailDraftType, p); return err }})
	}
	if d.GoogleActions.CalendarCreate != nil && a.RuleFor(CalendarCreateType) != spec.RuleDeny {
		out = append(out, Type{Name: CalendarCreateType,
			Description: "Create one event in the configured Google Calendar. Start and end are RFC 3339 timestamps.",
			ParamsSchema: map[string]any{
				"type": "object", "properties": map[string]any{
					"summary":     map[string]any{"type": "string", "minLength": 1, "maxLength": 255},
					"description": map[string]any{"type": "string", "maxLength": maxGoogleText},
					"location":    map[string]any{"type": "string", "maxLength": 512},
					"start":       map[string]any{"type": "string", "format": "date-time"},
					"end":         map[string]any{"type": "string", "format": "date-time"},
					"time_zone":   map[string]any{"type": "string", "maxLength": 128, "description": "Optional IANA time zone for the event."},
					"attendees":   map[string]any{"type": "array", "items": emailSchema(), "maxItems": maxGoogleAttendees},
				}, "required": []string{"summary", "start", "end"}, "additionalProperties": false,
			}, Validate: func(p json.RawMessage) error { _, err := googleCall(d, CalendarCreateType, p); return err }})
	}
	return out
}

func emailSchema() map[string]any {
	return map[string]any{"type": "string", "format": "email", "maxLength": 320}
}

// googleCall validates and converts one typed action to the configured
// connector tool's Google API argument shape.
func googleCall(d spec.Deployment, typ string, raw json.RawMessage) (GoogleCall, error) {
	if d.GoogleActions == nil {
		return GoogleCall{}, fmt.Errorf("%s: google_actions is not configured", typ)
	}
	switch typ {
	case GmailDraftType:
		binding := d.GoogleActions.GmailDraft
		if binding == nil {
			return GoogleCall{}, fmt.Errorf("%s: no gmail_draft binding is configured", typ)
		}
		var p gmailDraftParams
		if err := strictDecode(raw, &p); err != nil {
			return GoogleCall{}, err
		}
		if len(p.To) == 0 || len(p.To) > 20 || len(p.CC) > 20 || len(p.BCC) > 20 {
			return GoogleCall{}, fmt.Errorf("gmail:draft: to needs 1 to 20 addresses; cc and bcc allow at most 20 each")
		}
		if err := validateAddresses("gmail:draft", append(append(append([]string{}, p.To...), p.CC...), p.BCC...)); err != nil {
			return GoogleCall{}, err
		}
		if strings.TrimSpace(p.Subject) == "" || len(p.Subject) > 255 || strings.ContainsAny(p.Subject, "\r\n") {
			return GoogleCall{}, fmt.Errorf("gmail:draft: subject must be 1 to 255 bytes without line breaks")
		}
		if len(p.Body) == 0 || len(p.Body) > maxGoogleText {
			return GoogleCall{}, fmt.Errorf("gmail:draft: body must be 1 to %d bytes", maxGoogleText)
		}
		rawMessage := makeMail(p)
		args, _ := json.Marshal(struct {
			UserID  string `json:"userId"`
			Message struct {
				Raw string `json:"raw"`
			} `json:"message"`
		}{UserID: "me", Message: struct {
			Raw string `json:"raw"`
		}{Raw: base64.RawURLEncoding.EncodeToString(rawMessage)}})
		return GoogleCall{Connector: binding.Connector, Tool: binding.Tool, Arguments: args}, nil
	case CalendarCreateType:
		binding := d.GoogleActions.CalendarCreate
		if binding == nil {
			return GoogleCall{}, fmt.Errorf("%s: no calendar_create binding is configured", typ)
		}
		var p calendarCreateParams
		if err := strictDecode(raw, &p); err != nil {
			return GoogleCall{}, err
		}
		if strings.TrimSpace(p.Summary) == "" || len(p.Summary) > 255 || len(p.Description) > maxGoogleText || len(p.Location) > 512 {
			return GoogleCall{}, fmt.Errorf("calendar:create: summary must be 1 to 255 bytes; description and location must fit their limits")
		}
		start, err := time.Parse(time.RFC3339, p.Start)
		if err != nil {
			return GoogleCall{}, fmt.Errorf("calendar:create: start must be RFC 3339: %w", err)
		}
		end, err := time.Parse(time.RFC3339, p.End)
		if err != nil || !end.After(start) {
			return GoogleCall{}, fmt.Errorf("calendar:create: end must be an RFC 3339 timestamp after start")
		}
		if len(p.Attendees) > maxGoogleAttendees {
			return GoogleCall{}, fmt.Errorf("calendar:create: attendees allow at most %d addresses", maxGoogleAttendees)
		}
		if err := validateAddresses("calendar:create", p.Attendees); err != nil {
			return GoogleCall{}, err
		}
		if p.TimeZone != "" {
			if _, err := time.LoadLocation(p.TimeZone); err != nil {
				return GoogleCall{}, fmt.Errorf("calendar:create: time_zone must be an IANA time zone: %w", err)
			}
		}
		type dateTime struct {
			DateTime string `json:"dateTime"`
			TimeZone string `json:"timeZone,omitempty"`
		}
		type attendee struct {
			Email string `json:"email"`
		}
		event := map[string]any{"summary": p.Summary, "start": dateTime{p.Start, p.TimeZone}, "end": dateTime{p.End, p.TimeZone}}
		if p.Description != "" {
			event["description"] = p.Description
		}
		if p.Location != "" {
			event["location"] = p.Location
		}
		if len(p.Attendees) > 0 {
			attendees := make([]attendee, len(p.Attendees))
			for i, addr := range p.Attendees {
				attendees[i] = attendee{Email: addr}
			}
			event["attendees"] = attendees
		}
		calendarID := binding.CalendarID
		if calendarID == "" {
			calendarID = "primary"
		}
		args, _ := json.Marshal(map[string]any{"calendarId": calendarID, "event": event})
		return GoogleCall{Connector: binding.Connector, Tool: binding.Tool, Arguments: args}, nil
	default:
		return GoogleCall{}, fmt.Errorf("unknown Google action %q", typ)
	}
}

func validateAddresses(action string, addresses []string) error {
	for _, value := range addresses {
		if value == "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s: invalid email address", action)
		}
		addr, err := mail.ParseAddress(value)
		if err != nil || addr.Address == "" {
			return fmt.Errorf("%s: invalid email address %q", action, value)
		}
	}
	return nil
}

func makeMail(p gmailDraftParams) []byte {
	var b bytes.Buffer
	addresses := func(values []string) string {
		out := make([]string, len(values))
		for i, value := range values {
			a, _ := mail.ParseAddress(value)
			out[i] = a.String()
		}
		return strings.Join(out, ", ")
	}
	fmt.Fprintf(&b, "To: %s\r\n", addresses(p.To))
	if len(p.CC) > 0 {
		fmt.Fprintf(&b, "Cc: %s\r\n", addresses(p.CC))
	}
	if len(p.BCC) > 0 {
		fmt.Fprintf(&b, "Bcc: %s\r\n", addresses(p.BCC))
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", p.Subject))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	encoded := base64.StdEncoding.EncodeToString([]byte(p.Body))
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.Bytes()
}
