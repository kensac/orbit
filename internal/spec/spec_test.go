package spec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load("../../examples")
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Agents["finance"]
	if a == nil || a.SHA == "" || a.Runner != "claude" || a.Timezone != "America/New_York" {
		t.Fatalf("finance = %+v", a)
	}
	if a.RuleFor("webhook:home-assistant") != RuleApprove || a.RuleFor("webhook:other") != RuleDeny {
		t.Fatal("rules: named types take their rule, unnamed ones are denied")
	}
}

func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const base = `
defaults: {runner: r, channels: [c]}
runners: {r: {kind: claude-code}}
channels: {c: {kind: log}}
`

func TestInvalidConfigFailsWithEveryProblem(t *testing.T) {
	dir := write(t, map[string]string{
		"orbit.yaml": base,
		"agents/bad.yaml": `
name: bad
charter: x
runner: nope
triggers: [{schedule: "not cron"}]
sources: [{name: s, kind: ftp}]
actions: {"webhook:missing": auto, notify: maybe}
notify: {channels: [ghost]}
`,
	})
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{`unknown runner "nope"`, "trigger 0", `unknown kind "ftp"`, "unknown endpoint", "rule must be", `unknown channel "ghost"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

func TestUnknownFieldsAndMismatchedNamesFail(t *testing.T) {
	if _, err := Load(write(t, map[string]string{"orbit.yaml": base, "agents/a.yaml": "name: a\ncharter: x\ntriggerz: []\n"})); err == nil {
		t.Error("an unknown field must fail the load, not be ignored")
	}
	if _, err := Load(write(t, map[string]string{"orbit.yaml": base, "agents/a.yaml": "name: b\ncharter: x\n"})); err == nil {
		t.Error("an agent's name must match its file")
	}
}

func TestSHAChangesWithTheSpec(t *testing.T) {
	a, _ := Load(write(t, map[string]string{"orbit.yaml": base, "agents/a.yaml": "name: a\ncharter: one\n"}))
	b, _ := Load(write(t, map[string]string{"orbit.yaml": base, "agents/a.yaml": "name: a\ncharter: two\n"}))
	if a.Agents["a"].SHA == b.Agents["a"].SHA {
		t.Fatal("different specs share a hash")
	}
}

func TestExpandEnv(t *testing.T) {
	env := func(k string) (string, bool) {
		if k == "SET" {
			return "v", true
		}
		return "", false
	}
	if got, err := ExpandEnv("a ${SET} b", env); err != nil || got != "a v b" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := ExpandEnv("${UNSET}", env); err == nil || !strings.Contains(err.Error(), "UNSET") {
		t.Fatalf("an unset variable must be an error, got %v", err)
	}
}

func TestSessionSpec(t *testing.T) {
	load := func(orbit, agent string) (*Config, error) {
		return Load(write(t, map[string]string{"orbit.yaml": orbit, "agents/a.yaml": "name: a\ncharter: x\n" + agent}))
	}
	cfg, err := load(base, "session: {resume: true}\nactions: {continue: approve}\n")
	if err != nil {
		t.Fatal(err)
	}
	if s := cfg.Agents["a"].Session; time.Duration(s.Idle) != DefaultSessionIdle || s.Live != 0 {
		t.Fatalf("session = %+v, want the default idle and no live process", s)
	}
	if cfg.Agents["a"].RuleFor("continue") != RuleApprove {
		t.Fatal("continue is an action type with a rule")
	}
	for _, c := range []struct{ orbit, agent, want string }{
		{base, "session: {resume: true, idle: 48h}\n", "session.idle"},
		{base, "session: {resume: true, idle: 1h, live: 2h}\n", "session.live must be between"},
		{base, "session: {resume: true, idle: 10m, live: 20m}\n", "must not exceed session.idle"},
		{base, "session: {live: 5m}\n", "need resume: true"},
		{"defaults: {runner: r, channels: [c]}\nrunners: {r: {kind: codex}}\nchannels: {c: {kind: log}}\n", "session: {resume: true}\n", "needs a claude-code runner"},
		{"defaults: {runner: r, channels: [c]}\nrunners: {r: {kind: claude-code, max_budget_usd: 1}}\nchannels: {c: {kind: log}}\n",
			"session: {resume: true, live: 5m}\n", "max_budget_usd"},
	} {
		if _, err := load(c.orbit, c.agent); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want %q", c.agent, err, c.want)
		}
	}
}
