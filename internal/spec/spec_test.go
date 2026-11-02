package spec

import (
	"os"
	"path/filepath"
	"slices"
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

func TestReservedAgentNamesFail(t *testing.T) {
	for name := range ReservedAgentNames {
		_, err := Load(write(t, map[string]string{"orbit.yaml": base, "agents/" + name + ".yaml": "name: " + name + "\ncharter: x\n"}))
		if err == nil || !strings.Contains(err.Error(), "name is reserved") {
			t.Errorf("agent %q: err = %v, want reserved", name, err)
		}
	}
}

func TestTransformsAreValidatedAtLoad(t *testing.T) {
	agent := func(transforms string) string {
		return "name: a\ncharter: x\nsources: [{name: s, kind: file, path: /x}]\ntransforms:\n" + transforms
	}
	for _, bad := range []string{
		"  - {name: t, source: ghost, kind: sure-monthly}\n",
		"  - {name: s, source: s, kind: sure-monthly}\n",
		"  - {name: t, source: s, kind: nope}\n",
		"  - {name: t, source: s, kind: json-filter}\n",
		"  - {name: t, source: s, kind: json-filter, params: {where: [{field: x, op: like}]}}\n",
	} {
		if _, err := Load(write(t, map[string]string{"orbit.yaml": base, "agents/a.yaml": agent(bad)})); err == nil || !strings.Contains(err.Error(), "transform") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
	cfg, err := Load(write(t, map[string]string{"orbit.yaml": base, "agents/a.yaml": agent(
		"  - {name: t, source: s, kind: json-filter, params: {where: [{field: x, op: gt, value: 1}]}}\n")}))
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Agents["a"]; !a.Withheld("s") || a.Withheld("t") {
		t.Fatal("a source only a transform reads is withheld from the model")
	}
}

func TestGatesAreValidatedAtLoad(t *testing.T) {
	for _, bad := range []string{
		"[{kind: sometimes}]",
		"[{kind: unchanged, n: 3}]",
		"[{kind: items_above, input: ghost}]",
		"[{kind: items_above, input: s, n: -1}]",
		"[{kind: precheck}]",
		"[{kind: precheck, command: 'true', timeout: 5m}]",
		"[{kind: precheck, command: 'true', input: s}]",
		"[{kind: unchanged, command: 'true'}]",
	} {
		_, err := Load(write(t, map[string]string{"orbit.yaml": base,
			"agents/a.yaml": "name: a\ncharter: x\nsources: [{name: s, kind: file, path: /x}]\ngates: " + bad + "\n"}))
		if err == nil || !strings.Contains(err.Error(), "gate 0") {
			t.Errorf("%s: err = %v", bad, err)
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

func TestGitHubBlock(t *testing.T) {
	agent := "name: m\ncharter: x\nactions: {github:merge: approve}\n"
	if _, err := Load(write(t, map[string]string{"orbit.yaml": base, "agents/m.yaml": agent})); err == nil ||
		!strings.Contains(err.Error(), "needs a github block") {
		t.Fatalf("merge without github = %v", err)
	}
	bad := base + "github: {app_id: 0, repositories: [nope]}\n"
	_, err := Load(write(t, map[string]string{"orbit.yaml": bad}))
	for _, want := range []string{"app_id is required", "private_key_env is required", `"nope" is not owner/name`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v, want %q", err, want)
		}
	}
	good := base + "github: {app_id: 7, private_key_env: K, repositories: [kensac/orbit]}\n"
	cfg, err := Load(write(t, map[string]string{"orbit.yaml": good, "agents/m.yaml": agent}))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Deployment.AllowsRepo("kensac/orbit") || cfg.Deployment.AllowsRepo("kensac/other") {
		t.Fatal("AllowsRepo: only listed repositories")
	}
	if cfg.Agents["m"].RuleFor("github:merge") != RuleApprove {
		t.Fatal("rule")
	}
}

func TestReposAreReadRepositoriesOnly(t *testing.T) {
	orbit := `
defaults: {runner: r, channels: [c]}
runners: {r: {kind: claude-code}, a: {kind: anthropic, api_key_env: K}}
channels: {c: {kind: log}}
github: {app_id: 7, private_key_env: K, repositories: [kensac/orbit], read_repositories: [kensac/orbit, kensac/homelab-config, other/orbit]}
`
	bad := `name: m
charter: x
runner: a
repos:
  - {repo: kensac/orbit, ref: main}
  - {repo: kensac/secret}
  - {repo: other/orbit}
  - {repo: kensac/homelab-config, ref: "--upload-pack=x"}
`
	_, err := Load(write(t, map[string]string{"orbit.yaml": orbit, "agents/m.yaml": bad}))
	for _, want := range []string{
		"kensac/secret is not in github.read_repositories",
		`a second repository named "orbit"`,
		`ref "--upload-pack=x" is not a branch`,
		`needs "github" in needs`,
		"an anthropic runner reads no files",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v, want %q", err, want)
		}
	}
	good := "name: m\ncharter: x\nneeds: [github]\nrepos: [{repo: kensac/homelab-config, ref: main}, {repo: kensac/orbit}]\n"
	cfg, err := Load(write(t, map[string]string{"orbit.yaml": orbit, "agents/m.yaml": good}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents["m"].Repos[0].Name() != "homelab-config" {
		t.Fatal("a repository's directory is its name")
	}
	// Reading a repository grants nothing to act on it.
	if cfg.Deployment.AllowsRepo("kensac/homelab-config") || !cfg.Deployment.AllowsRead("kensac/homelab-config") {
		t.Fatal("read_repositories must not widen repositories")
	}
	noGitHub := "name: m\ncharter: x\nneeds: [github]\nrepos: [{repo: kensac/orbit}]\n"
	if _, err := Load(write(t, map[string]string{"orbit.yaml": base, "agents/m.yaml": noGitHub})); err == nil ||
		!strings.Contains(err.Error(), "not in github.read_repositories") {
		t.Fatalf("repos without a github block = %v", err)
	}
}

const buildBase = `
defaults: {runner: r, channels: [c]}
runners: {r: {kind: claude-code}, x: {kind: codex}}
channels: {c: {kind: log}}
github: {app_id: 1, private_key_env: K, repositories: [kensac/orbit, kensac/other]}
`

func TestBuildSection(t *testing.T) {
	good := buildBase + `
build:
  sandboxd: 100.123.116.85:8091
  agent: b
  author: "orbit-kensac[bot] <351241810+orbit-kensac[bot]@users.noreply.github.com>"
  repositories:
    kensac/orbit: {commands: [go test, make check], protected: ["deploy/**"]}
`
	cfg, err := Load(write(t, map[string]string{"orbit.yaml": good, "agents/b.yaml": "name: b\ncharter: x\ntimeout: 45m\n"}))
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Deployment.Build
	r, ok := cfg.Deployment.Builds("kensac/orbit")
	if b.TokenEnv != "ORBIT_SANDBOX_TOKEN" || !ok || r.MaxDiffLines != DefaultMaxDiffLines || len(r.Argv()) != 2 || r.Argv()[1][1] != "check" {
		t.Fatalf("build = %+v %+v", b, r)
	}
	if _, ok := cfg.Deployment.Builds("kensac/other"); ok {
		t.Fatal("a repository without a policy is not built")
	}
	if name, email, err := ParseAuthor(b.Author); err != nil || name != "orbit-kensac[bot]" || !strings.HasPrefix(email, "351241810+") {
		t.Fatalf("author = %q %q %v", name, email, err)
	}

	bad := buildBase + `
build:
  sandboxd: nowhere
  agent: b
  author: orbit
  repositories:
    kensac/other: {commands: ["go test $(x)"], protected: ["/etc/**"], max_diff_lines: 999999, trusted_scripts: [ABC]}
    kensac/third: {commands: []}
`
	_, err = Load(write(t, map[string]string{"orbit.yaml": bad, "agents/b.yaml": "name: b\ncharter: x\nrunner: x\ntimeout: 2h\n"}))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"neither host:port", "must be \"Name <email>\"", "claude-code runner", "exceeds 50m0s",
		`"kensac/third" is not in github.repositories`, "only kensac/orbit", "is not a plain word", "not a relative glob",
		"max_diff_lines", "at least one command", `trusted script "ABC" is not a sha256`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if _, err := Load(write(t, map[string]string{"orbit.yaml": base + "build: {sandboxd: unix:/s, agent: b, author: \"A <a@b>\", repositories: {kensac/orbit: {commands: [ls]}}}\n",
		"agents/b.yaml": "name: b\ncharter: x\n"})); err == nil || !strings.Contains(err.Error(), "needs a github block") {
		t.Fatalf("build without github = %v", err)
	}
}

func TestLoadAllMergesTheDatabaseAndBuiltins(t *testing.T) {
	dir := write(t, map[string]string{"orbit.yaml": base, "agents/a.yaml": "name: a\ncharter: x\n"})
	cfg, err := LoadAll(dir, []Definition{{Name: "b", Version: 3, Enabled: false, Spec: []byte(`{"name":"b","charter":"y"}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Agents["a"]; a.Origin != OriginFile {
		t.Fatalf("a = %+v", a)
	}
	if b := cfg.Agents["b"]; b.Origin != OriginDatabase || b.Version != 3 || b.IsEnabled() || b.Runner != "r" {
		t.Fatalf("b = %+v: defaults apply to database agents", b)
	}
	if o := cfg.Agents[BuiltinName]; o == nil || o.Origin != OriginBuiltin || o.Runner != "r" {
		t.Fatalf("builtin = %+v", o)
	}
	if _, err := LoadAll(dir, []Definition{{Name: "a", Version: 1, Enabled: true, Spec: []byte(`{"name":"a","charter":"y"}`)}}); err == nil ||
		!strings.Contains(err.Error(), "defined twice") {
		t.Fatalf("a name in both = %v", err)
	}
	if _, err := LoadAll(dir, []Definition{{Name: "c", Version: 1, Enabled: true, Spec: []byte(`{"name":"c","charter":"y","runner":"none"}`)}}); err == nil {
		t.Fatal("a database agent is validated like a file")
	}
	// A file agent named like the builtin replaces it.
	cfg, err = LoadAll(write(t, map[string]string{"orbit.yaml": base, "agents/orbit.yaml": "name: orbit\ncharter: mine\n"}), nil)
	if err != nil || cfg.Agents[BuiltinName].Origin != OriginFile {
		t.Fatalf("override = %v %+v", err, cfg)
	}
}

func TestContextFieldsAreValidatedAtLoad(t *testing.T) {
	good := "topics: [finance]\nskills: [close]\nlearn: {idle: 1h}\nhooks:\n  - {event: PreToolUse, matcher: WebFetch, deny: no}\n  - {event: Stop, say: check}\n  - {event: PostToolUse, run: [gofmt, -l, .], timeout: 5s}\n"
	if _, err := Load(write(t, map[string]string{"orbit.yaml": base, "agents/a.yaml": "name: a\ncharter: x\n" + good})); err != nil {
		t.Fatalf("good: %v", err)
	}
	for _, bad := range []string{
		"topics: [Finance]",
		"topics: [a, a]",
		"skills: [Bad]",
		"learn: {idle: 1m}",
		"learn: {runner: ghost}",
		"hooks: [{event: Notification, say: x}]",
		"hooks: [{event: Stop, matcher: X, say: x}]",
		"hooks: [{event: Stop, deny: x}]",
		"hooks: [{event: Stop, say: x, run: [y]}]",
		"hooks: [{event: Stop}]",
		"hooks: [{event: Stop, say: x, timeout: 5s}]",
		"hooks: [{event: Stop, run: [y], timeout: 2m}]",
	} {
		if _, err := Load(write(t, map[string]string{"orbit.yaml": base, "agents/a.yaml": "name: a\ncharter: x\n" + bad + "\n"})); err == nil {
			t.Errorf("%s: loaded", bad)
		}
	}
}

func TestDelegatesAreResolvedAndChecked(t *testing.T) {
	cfg, err := Load(write(t, map[string]string{
		"orbit.yaml":       base,
		"agents/boss.yaml": "name: boss\ncharter: x\ndelegates: [\"*\"]\nactions: {delegate: approve, task:cancel: auto}\n",
		"agents/one.yaml":  "name: one\ncharter: x\n",
	}))
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Agents["boss"]
	if got := b.DelegateTargets(); !slices.Contains(got, "boss") || !slices.Contains(got, "one") || !slices.IsSorted(got) {
		t.Fatalf("targets = %v", got)
	}
	if b.RuleFor("task:message") != RuleApprove || b.RuleFor("task:cancel") != RuleAuto {
		t.Fatal("task types follow the delegate rule unless named")
	}
	_, err = Load(write(t, map[string]string{
		"orbit.yaml":       base,
		"agents/boss.yaml": "name: boss\ncharter: x\ndelegates: [nobody]\n",
		"agents/two.yaml":  "name: two\ncharter: x\nactions: {delegate: auto}\n",
	}))
	for _, want := range []string{`delegates: "nobody" is not a configured agent`, `agent "two": action "delegate": needs delegates`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v lacks %q", err, want)
		}
	}
}

func TestTriggersTakeCronOrAnRRule(t *testing.T) {
	load := func(triggers string) error {
		_, err := Load(write(t, map[string]string{"orbit.yaml": base,
			"agents/a.yaml": "name: a\ncharter: x\ntimezone: America/New_York\ntriggers: " + triggers + "\n"}))
		return err
	}
	for _, ok := range []string{
		`[{schedule: "0 9 * * *"}]`,
		`[{rrule: "FREQ=WEEKLY;INTERVAL=2;BYDAY=TU", start: "2026-11-03T09:00"}]`,
		`[{rrule: "RRULE:FREQ=MONTHLY;BYDAY=-1FR", start: "2026-11-01 17:30"}]`,
	} {
		if err := load(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for bad, want := range map[string]string{
		`[{}]`: "schedule or rrule is required",
		`[{schedule: "0 9 * * *", rrule: "FREQ=DAILY", start: "2026-11-03T09:00"}]`: "schedule and rrule cannot both",
		`[{rrule: "FREQ=DAILY"}]`:                                  "an rrule needs a start",
		`[{rrule: "FREQ=DAILY", start: "tomorrow"}]`:               `start "tomorrow": want local time`,
		`[{rrule: "FREQ=FORTNIGHTLY", start: "2026-11-03T09:00"}]`: "rrule: FREQ=FORTNIGHTLY",
		`[{schedule: "0 9 * * *", start: "2026-11-03T09:00"}]`:     "start is for an rrule",
	} {
		if err := load(bad); err == nil || !strings.Contains(err.Error(), "trigger 0: "+want) {
			t.Errorf("%s: err = %v, want %q", bad, err, want)
		}
	}
}

func TestAPrecheckIsForFileAgentsOnly(t *testing.T) {
	dir := write(t, map[string]string{"orbit.yaml": base, "agents/a.yaml": "name: a\ncharter: x\ngates: [{kind: precheck, command: 'exit 0'}]\n"})
	if _, err := LoadAll(dir, nil); err != nil {
		t.Fatalf("a file agent's precheck: %v", err)
	}
	_, err := LoadAll(dir, []Definition{{Name: "b", Version: 1, Enabled: true,
		Spec: []byte(`{"name":"b","charter":"y","gates":[{"kind":"precheck","command":"cat /proc/1/environ"}]}`)}})
	if err == nil || !strings.Contains(err.Error(), "only an agent in the configuration files") {
		t.Fatalf("a database agent's precheck = %v", err)
	}
}
