package engine

import "testing"

func TestParseRemember(t *testing.T) {
	for body, want := range map[string]rememberCmd{
		"/remember Never push while a question is open.":            {Type: "feedback", Text: "Never push while a question is open."},
		"/remember --agent Keep replies to three lines":             {Agent: true, Type: "feedback", Text: "Keep replies to three lines"},
		"/remember --topic homelab --project hetzner has 32 GB":     {Topic: "homelab", Type: "project", Text: "hetzner has 32 GB"},
		"/remember --topic=money\nRent is fixed.\nSecond line.":     {Topic: "money", Type: "feedback", Text: "Rent is fixed.\nSecond line."},
		"/remember I live in New York":                              {Type: "user", Text: "I live in New York"},
		"/remember my job is data engineering":                      {Type: "user", Text: "my job is data engineering"},
		"/remember The runbooks are at https://example.com/runbook": {Type: "reference", Text: "The runbooks are at https://example.com/runbook"},
		"/remember Our launch deadline is Friday":                   {Type: "project", Text: "Our launch deadline is Friday"},
		"  /remember   --user   Prefers tea  ":                      {Type: "user", Text: "Prefers tea"},
	} {
		got, err := parseRemember(body)
		if err != nil || got != want {
			t.Errorf("parseRemember(%q) = %+v, %v; want %+v", body, got, err, want)
		}
	}
	for _, bad := range []string{"/remember", "/remember --agent", "/remember --topic", "/remember --nope x", "/remember --agent --topic a x", "/remember --user --project x"} {
		if _, err := parseRemember(bad); err == nil {
			t.Errorf("parseRemember(%q) took it", bad)
		}
	}
	for body, want := range map[string]bool{"/remember x": true, "/remember\nx": true, "/remember": true, "/remembered x": false, "please /remember x": false} {
		if isRemember(body) != want {
			t.Errorf("isRemember(%q) = %v", body, !want)
		}
	}
}

func TestRememberTitle(t *testing.T) {
	for text, want := range map[string]string{
		"Never push while a question is open. Ask first.": "Never push while a question is open",
		"Rent is fixed\nat 2700":                          "Rent is fixed",
		"One two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen": "One two three four five six seven eight nine ten eleven twelve thirteen...",
	} {
		if got := rememberTitle(text); got != want || len(got) > rememberTitleMax {
			t.Errorf("rememberTitle(%q) = %q, want %q", text, got, want)
		}
	}
}
