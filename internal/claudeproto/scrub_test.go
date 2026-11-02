package claudeproto

import (
	"encoding/json"
	"strings"
	"testing"
)

// fake joins a made-up credential from parts, so the repository's secret scanner does not take a test value for a real one.
func fake(parts ...string) string { return strings.Join(parts, "") }

func TestScrubCredentialsKeepsNamesAndJSON(t *testing.T) {
	cases := []struct{ in, want string }{
		{"export CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-abcdefghijklmnopqrstuvwxyz", "export CLAUDE_CODE_OAUTH_TOKEN=" + Redacted},
		{"DB_PASSWORD: correct-horse-battery", "DB_PASSWORD: " + Redacted},
		{`{"client_secret":"` + fake("0123", "456789abcdef") + `"}`, `{"client_secret":"` + Redacted + `"}`},
		{`{"command":"curl -H \"Authorization: Bearer abcdefghijklmnop1234\" x"}`, `{"command":"curl -H \"Authorization: Bearer ` + Redacted + `\" x"}`},
		{"see https://user:hunter22@example.com/x", "see https://user:" + Redacted + "@example.com/x"},
		{"token " + fake("ghp_", "0123456789abcdefghijklmnopqrstuvwxyzAB") + " here", "token " + Redacted + " here"},
		{"key " + fake("AKIA", "ABCDEFGHIJKLMNOP") + ".", "key " + Redacted + "."},
		{`"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----"`, `"` + Redacted + `"`},
		// Not credentials: short values, references, counts.
		{"PASSWORD=short", "PASSWORD=short"},
		{"API_KEY=${HETZNER_API_KEY}", "API_KEY=${HETZNER_API_KEY}"},
		{`{"input_tokens":123456789,"max_tokens":40000000}`, `{"input_tokens":123456789,"max_tokens":40000000}`},
	}
	for _, c := range cases {
		if got := ScrubCredentials(c.in); got != c.want {
			t.Errorf("ScrubCredentials(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}

func TestScrubCredentialsLeavesJSONValid(t *testing.T) {
	line := `{"type":"user","message":{"content":"my ANTHROPIC_API_KEY='sk-ant-api03-zzzzzzzzzzzzzzzzzzzzzzzz' and \"password\": \"p4ssw0rd!!\""}}`
	out := ScrubCredentials(line)
	if !json.Valid([]byte(out)) {
		t.Fatalf("scrubbed line is not JSON: %s", out)
	}
	if strings.Contains(out, "sk-ant") || strings.Contains(out, "p4ssw0rd") {
		t.Fatalf("a credential survived: %s", out)
	}
}
