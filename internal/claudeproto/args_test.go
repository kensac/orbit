package claudeproto

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestArgs(t *testing.T) {
	cases := []struct {
		t    Transport
		want string
	}{
		{Transport{}, "-p --output-format stream-json --verbose --model m"},
		{Transport{Ephemeral: true}, "-p --output-format stream-json --verbose --no-session-persistence --model m"},
		{Transport{Stream: true, PermissionPrompt: true, Resume: "s1"},
			"-p --output-format stream-json --verbose --input-format stream-json --permission-prompt-tool stdio --resume s1 --fork-session --model m"},
	}
	for _, c := range cases {
		got, err := Args(c.t, []string{"--model", "m"})
		if err != nil || strings.Join(got, " ") != c.want {
			t.Errorf("%+v: %q %v, want %q", c.t, got, err, c.want)
		}
	}
	for _, bad := range []Transport{{PermissionPrompt: true}, {Ephemeral: true, Resume: "s"}} {
		if _, err := Args(bad, nil); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestArgsRefuseTheTransportsFlags(t *testing.T) {
	for _, extra := range [][]string{
		{"--resume", "x"}, {"-r", "x"}, {"--input-format=text"}, {"--fork-session"}, {"-p"},
		{"--model", "m", "--permission-prompt-tool", "mcp__x"}, {"--continue"}, {"--session-id", "x"},
	} {
		if _, err := Args(Transport{}, extra); err == nil {
			t.Errorf("%q accepted", extra)
		}
	}
	// Values, and anything after --, are not flags.
	for _, extra := range [][]string{
		{"--append-system-prompt", "resume the work"}, {"--model", "m"}, {"--", "--resume"}, {"--tools", "Read,Glob"},
	} {
		if _, err := Args(Transport{}, extra); err != nil {
			t.Errorf("%q refused: %v", extra, err)
		}
	}
}

func TestResumeArgs(t *testing.T) {
	if ResumeArgs("") != nil {
		t.Error("an empty id resumes")
	}
	if got := ResumeArgs("s1"); !slices.Equal(got, []string{"--resume", "s1", "--fork-session"}) {
		t.Errorf("every resume forks: %q", got)
	}
}

func TestEnvKeepsOneCredential(t *testing.T) {
	env := []string{
		"HOME=/h", "PATH=/bin", OAuthToken + "=tok", APIKey + "=key", "ANTHROPIC_AUTH_TOKEN=x",
		"AWS_BEARER_TOKEN_BEDROCK=y", "CLAUDECODE=1", "CLAUDE_CODE_SESSION_ID=parent",
		"CLAUDE_CODE_ENTRYPOINT=cli", "ANTHROPIC_CUSTOM_HEADERS=Authorization: Bearer z", "LANG=C.UTF-8", "LANG=en_US.UTF-8",
	}
	got := Env(env, OAuthToken)
	want := []string{"HOME=/h", "PATH=/bin", OAuthToken + "=tok", "LANG=en_US.UTF-8"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := Env(env, ""); slices.ContainsFunc(got, func(kv string) bool { return strings.Contains(kv, "tok") || strings.Contains(kv, "key") }) {
		t.Fatalf("no credential asked for, one kept: %q", got)
	}
	if got := Env([]string{"ANTHROPIC_CUSTOM_HEADERS=X-Trace: 1"}, ""); len(got) != 1 {
		t.Fatalf("a header without a credential is dropped: %q", got)
	}
}

func TestCredential(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(n string) (string, bool) { v, ok := m[n]; return v, ok }
	}
	if c := Credential(env(map[string]string{OAuthToken: "t", APIKey: "k"})); c != OAuthToken {
		t.Errorf("with both, %s: the key must never displace the subscription", c)
	}
	if c := Credential(env(map[string]string{OAuthToken: "", APIKey: "k"})); c != APIKey {
		t.Errorf("an empty token is no token: %s", c)
	}
	if c := Credential(env(nil)); c != "" {
		t.Errorf("none: %s", c)
	}
}

func TestEnvDropsAKeyThePersonRejected(t *testing.T) {
	key := "sk-ant-api03-" + strings.Repeat("a", 30) + "LAST20CHARACTERS1234"
	write := func(path, rejected string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"customApiKeyResponses":{"rejected":["`+rejected+`"],"approved":[]}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hasKey := func(env []string) bool { return slices.Contains(env, APIKey+"="+key) }

	home := t.TempDir()
	env := []string{"HOME=" + home, APIKey + "=" + key}
	if !hasKey(Env(env, APIKey)) {
		t.Fatal("a key with no config is kept")
	}
	write(filepath.Join(home, ".claude.json"), "someotherkeyxxxxxxxx")
	if !hasKey(Env(env, APIKey)) {
		t.Fatal("a key someone else rejected is kept")
	}
	write(filepath.Join(home, ".claude.json"), "LAST20CHARACTERS1234")
	if hasKey(Env(env, APIKey)) {
		t.Fatal("a rejected key is kept")
	}

	// CLAUDE_CONFIG_DIR moves the config, and a legacy .config.json wins.
	dir := filepath.Join(t.TempDir(), "cfg")
	env = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + dir, APIKey + "=" + key}
	if !hasKey(Env(env, APIKey)) {
		t.Fatal("the config under HOME was read although CLAUDE_CONFIG_DIR is set")
	}
	write(filepath.Join(dir, ".claude.json"), "LAST20CHARACTERS1234")
	if hasKey(Env(env, APIKey)) {
		t.Fatal("the rejection under CLAUDE_CONFIG_DIR was missed")
	}
	write(filepath.Join(dir, ".config.json"), "nothing")
	if !hasKey(Env(env, APIKey)) {
		t.Fatal(".config.json is the config when present")
	}
}

func TestParseLineAndResult(t *testing.T) {
	for raw, kind := range map[string]string{
		`{"type":"system","subtype":"init"}`: "system.init", `{"type":"assistant"}`: "assistant",
		`not json`: "raw", `{"subtype":"x"}`: "raw", `[1]`: "raw",
	} {
		if l := ParseLine([]byte(raw)); l.Kind != kind {
			t.Errorf("%s: %s, want %s", raw, l.Kind, kind)
		}
	}
	if id := ParseLine([]byte(`{"type":"system","session_id":"s"}`)).SessionID(); id != "s" {
		t.Errorf("session id %q", id)
	}
	login, _ := ParseResult([]byte(`{"type":"result","subtype":"success","is_error":true,"result":"Invalid API key\nPlease run /login"}`))
	if login.Failure() != "Invalid API key\nPlease run /login" {
		t.Errorf("a login failure is reported in result: %q", login.Failure())
	}
	turns, _ := ParseResult([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"errors":[" too many turns ",""," budget "]}`))
	if turns.Failure() != "too many turns; budget" {
		t.Errorf("other failures are in errors: %q", turns.Failure())
	}
	ok, _ := ParseResult([]byte(`{"type":"result","subtype":"success","structured_output":null}`))
	if ok.Failure() != "" || ok.HasOutput() {
		t.Errorf("a null output is no output")
	}
	if _, err := ParseResult([]byte(`{`)); err == nil {
		t.Error("a broken result parsed")
	}
}
