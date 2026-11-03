package engine_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/build"
	"github.com/kensac/orbit/internal/build/buildtest"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

type buildEnv struct {
	e    *engine.Engine
	s    *store.Store
	gh   *githubtest.Server
	sb   *buildtest.Sandboxd
	bare string
}

// newBuildEnv is a deployment that builds kensac/orbit with agent orbit:
// the fake GitHub, a bare repository for its git side, a fake sandboxd
// running the fake CLI.
func newBuildEnv(t *testing.T) *buildEnv {
	t.Helper()
	return newBuildEnvWith(t, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-engine-test-credential-000000"})
}

// newBuildEnvWith is newBuildEnv with creds as orbit's backend credentials.
func newBuildEnvWith(t *testing.T, creds map[string]string) *buildEnv {
	t.Helper()
	b := &buildEnv{s: testdb.New(t), gh: githubtest.New(t), sb: buildtest.NewSandboxd(t), bare: buildtest.Repo(t)}
	b.gh.Mu.Lock()
	b.gh.PR = nil
	b.gh.BranchHead = func(ref string) string { return buildtest.RevParse(b.bare, "refs/heads/"+ref) }
	b.gh.Mu.Unlock()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"orbit.yaml": fmt.Sprintf(`
defaults: {runner: claude, channels: [log]}
runners: {claude: {kind: claude-code, model: m, max_turns: 40}}
channels: {log: {kind: log}}
github: {app_id: 42, private_key_env: KEY, api_url: %s, repositories: [kensac/orbit]}
build:
  sandboxd: %s
  agent: orbit
  author: "orbit-kensac[bot] <351241810+orbit-kensac[bot]@users.noreply.github.com>"
  repositories:
    kensac/orbit: {commands: [go test, make check], protected: ["deploy/**"]}
`, b.gh.URL(), strings.TrimPrefix(b.sb.URL(), "http://")),
		"agents/orbit.yaml": "name: orbit\ncharter: build orbit\ntimeout: 2m\nactions: {github:merge: approve}\n",
	} {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"KEY": githubtest.KeyPEM(), "ORBIT_SANDBOX_TOKEN": buildtest.SandboxToken}
	for k, v := range creds {
		env[k] = v
	}
	b.e = &engine.Engine{
		Store: b.s, DataDir: t.TempDir(), Client: b.gh.Srv.Client(), Secret: []byte("secret"),
		Env: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) {
			t.Fatal("a build run must not call the agent's ordinary runner")
			return nil, nil
		},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Builder: &build.Runner{WorkRoot: t.TempDir(), CloneURL: func(string) string { return b.bare }},
		Drain:   engine.NewDrain(),
	}
	if err := b.e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	specs := map[string]store.AgentSpec{}
	for n, a := range cfg.Agents {
		j, _ := json.Marshal(a)
		specs[n] = store.AgentSpec{SHA: a.SHA, JSON: j, Enabled: true}
	}
	if err := b.s.SyncAgents(ctx, specs); err != nil {
		t.Fatal(err)
	}
	return b
}

// queueBuild files, plans, approves and enqueues a build whose plan carries
// the fake CLI's script.
func (b *buildEnv) queueBuild(t *testing.T, steps ...map[string]any) (ticket, run string) {
	t.Helper()
	sha := b.e.Current().Config.Agents["orbit"].SHA
	r, _, err := b.s.CreateTicket(ctx, store.NewTicket{Kind: "request", Title: "Add hello", Body: "please", Agent: "orbit",
		SpecSHA: sha, Repo: "kensac/orbit", Author: "kanishk"})
	if err != nil {
		t.Fatal(err)
	}
	script, _ := json.Marshal(map[string]any{"steps": steps})
	if _, err := b.s.ProposePlan(ctx, r.TicketID, "", "Add hello.txt.\n\nFAKE-SCRIPT: "+string(script)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.s.ApprovePlan(ctx, r.TicketID, 1, "kanishk", ""); err != nil {
		t.Fatal(err)
	}
	run, err = b.s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "orbit", SpecSHA: sha, Purpose: "build"})
	if err != nil {
		t.Fatal(err)
	}
	return r.TicketID, run
}

func writeStep(path, content string) map[string]any {
	return map[string]any{"tool": "Write", "input": map[string]any{"file_path": path, "content": content}}
}

func TestABuildRunOpensItsPRAndMovesItsTicket(t *testing.T) {
	b := newBuildEnv(t)
	tid, run := b.queueBuild(t, writeStep("/work/hello.txt", "hi\n"), writeStep("/work/deploy/x", "no"))
	c, err := b.s.ClaimRun(ctx, "w")
	if err != nil || c.ID != run {
		t.Fatalf("claim %+v %v", c, err)
	}
	if err := b.e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	r, _ := b.s.GetRun(ctx, run)
	if r.Status != "succeeded" || r.Report == nil || !strings.Contains(*r.Report, "PR #7") || *r.CostUSD != 0.5 {
		t.Fatalf("run = %+v err %v", r, r.Error)
	}
	tk, _ := b.s.GetTicket(ctx, tid)
	head := buildtest.RevParse(b.bare, "refs/heads/orbit/"+strings.ReplaceAll(tk.ID, "-", "")[:8])
	if tk.Status != store.TicketReview || *tk.PRNumber != 7 || tk.HeadSHA == nil || *tk.HeadSHA != head || tk.BaseSHA == nil {
		t.Fatalf("ticket = %+v, branch head %s", tk, head)
	}
	ds, _ := b.s.Decisions(ctx, run)
	if len(ds) != 2 || !ds[0].Allowed || ds[1].Allowed || ds[0].Attempt != c.Attempt {
		t.Fatalf("decisions = %+v", ds)
	}
	if open, _ := b.s.OpenSandboxes(ctx, run); len(open) != 0 {
		t.Fatalf("sandboxes left open: %v", open)
	}
	arts, _ := b.s.ListArtifacts(ctx, run)
	names := ""
	for _, a := range arts {
		names += a.Direction + "/" + a.Name + " "
	}
	if !strings.Contains(names, "out/diff.patch") || !strings.Contains(names, "out/output.json") {
		t.Fatalf("artifacts = %s", names)
	}
}

// A build's sandbox gets the credential an ordinary run would: the subscription token when set, else the API key, never both and never another auth variable.
func TestABuildGetsTheOrdinaryRunsCredential(t *testing.T) {
	const tok, key = "sk-ant-oat01-engine-test-token", "sk-ant-api03-engine-test-key"
	for name, tc := range map[string]struct {
		creds map[string]string
		want  map[string]string
	}{
		"token":         {map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tok}, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tok}},
		"api key":       {map[string]string{"ANTHROPIC_API_KEY": key}, map[string]string{"ANTHROPIC_API_KEY": key}},
		"token and key": {map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tok, "ANTHROPIC_API_KEY": key, "ANTHROPIC_AUTH_TOKEN": "x"}, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": tok}},
	} {
		t.Run(name, func(t *testing.T) {
			b := newBuildEnvWith(t, tc.creds)
			_, run := b.queueBuild(t, writeStep("/work/hello.txt", "hi\n"))
			c, _ := b.s.ClaimRun(ctx, "w")
			if err := b.e.Execute(ctx, c); err != nil {
				t.Fatal(err)
			}
			if r, _ := b.s.GetRun(ctx, run); r.Status != "succeeded" {
				t.Fatalf("run %s: %v", r.Status, r.Error)
			}
			if at := b.sb.Attaches(); len(at) != 1 || !reflect.DeepEqual(at[0].Env, tc.want) {
				t.Fatalf("attaches = %+v, want env %v", at, tc.want)
			}
		})
	}
}

func TestABuildWithoutACredentialFailsNamingBoth(t *testing.T) {
	b := newBuildEnvWith(t, map[string]string{"ANTHROPIC_AUTH_TOKEN": "x"})
	tid, _ := b.queueBuild(t, writeStep("/work/hello.txt", "hi\n"))
	c, _ := b.s.ClaimRun(ctx, "w")
	if err := b.e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	tk, _ := b.s.GetTicket(ctx, tid)
	if tk.Error == nil || !strings.Contains(*tk.Error, "CLAUDE_CODE_OAUTH_TOKEN or ANTHROPIC_API_KEY") {
		t.Fatalf("ticket = %+v", tk)
	}
	if n, _, _ := b.sb.Counts(); n != 0 {
		t.Fatalf("%d sandboxes created without a credential", n)
	}
}

func TestAFailedBuildFailsItsTicket(t *testing.T) {
	b := newBuildEnv(t)
	tid, _ := b.queueBuild(t, map[string]any{"sneak": "/work/.github/workflows/x.yml", "content": "x"})
	c, _ := b.s.ClaimRun(ctx, "w")
	if err := b.e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	tk, _ := b.s.GetTicket(ctx, tid)
	if tk.Status != store.TicketFailed || !strings.Contains(*tk.Error, "protected path") {
		t.Fatalf("ticket = %+v", tk)
	}
	if r, _ := b.s.GetRun(ctx, c.ID); r.Status != "failed" {
		t.Fatalf("run %s", r.Status)
	}
}

func TestADrainingInstanceHandsTheBuildBack(t *testing.T) {
	b := newBuildEnv(t)
	tid, run := b.queueBuild(t, writeStep("/work/hello.txt", "hi\n"))
	c, _ := b.s.ClaimRun(ctx, "w")
	b.e.Drain.Start()
	if err := b.e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if r, _ := b.s.GetRun(ctx, run); r.Status != "queued" {
		t.Fatalf("run %s, want handed back", r.Status)
	}
	if created, _, _ := b.sb.Counts(); created != 0 {
		t.Fatal("a draining instance started a sandbox")
	}
	if tk, _ := b.s.GetTicket(ctx, tid); tk.Status != store.TicketBuilding {
		t.Fatalf("ticket %s", tk.Status)
	}
}

func TestAnInterruptedBuildsSandboxIsDestroyedBeforeTheNext(t *testing.T) {
	b := newBuildEnv(t)
	tid, run := b.queueBuild(t, writeStep("/work/hello.txt", "hi\n"))
	// Attempt 1 created its sandbox and died with it up.
	c1, _ := b.s.ClaimRun(ctx, "w")
	cl := b.sb.Client(t)
	left, err := cl.Create(ctx, run, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.s.RecordSandbox(ctx, run, c1.Attempt, left.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.s.HandBack(ctx, run, c1.Attempt, nil, nil); err != nil {
		t.Fatal(err)
	}
	c2, _ := b.s.ClaimRun(ctx, "w")
	if c2.Attempt != c1.Attempt+1 {
		t.Fatalf("attempt %d", c2.Attempt)
	}
	if err := b.e.Execute(ctx, c2); err != nil {
		t.Fatal(err)
	}
	if _, ok := b.sb.Dir(left.ID); ok {
		t.Fatal("the interrupted attempt's sandbox survived")
	}
	if created, destroyed, open := b.sb.Counts(); created != 2 || destroyed != 2 || open != 0 {
		t.Fatalf("created %d destroyed %d open %d", created, destroyed, open)
	}
	if tk, _ := b.s.GetTicket(ctx, tid); tk.Status != store.TicketReview {
		t.Fatalf("ticket %s", tk.Status)
	}
}

func TestAStackedBuildStartsFromTheTicketBelow(t *testing.T) {
	b := newBuildEnv(t)
	below, run := b.queueBuild(t, writeStep("/work/hello.txt", "hi\n"))
	c, _ := b.s.ClaimRun(ctx, "w")
	if err := b.e.Execute(ctx, c); err != nil || c.ID != run {
		t.Fatal(err)
	}
	parent, _ := b.s.GetTicket(ctx, below)
	sha := b.e.Current().Config.Agents["orbit"].SHA
	r, _, err := b.s.CreateTicket(ctx, store.NewTicket{Kind: "request", Title: "On top", Body: "next", Agent: "orbit",
		SpecSHA: sha, Repo: "kensac/orbit", StackOn: below})
	if err != nil {
		t.Fatal(err)
	}
	script, _ := json.Marshal(map[string]any{"steps": []map[string]any{writeStep("/work/next.txt", "next\n")}})
	_, _ = b.s.ProposePlan(ctx, r.TicketID, "", "Add next.txt.\n\nFAKE-SCRIPT: "+string(script))
	_, _ = b.s.ApprovePlan(ctx, r.TicketID, 1, "kanishk", "")
	childRun, err := b.s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "orbit", SpecSHA: sha, Purpose: "build"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ = b.s.ClaimRun(ctx, "w")
	if c == nil || c.ID != childRun {
		t.Fatalf("claimed %+v, want the stacked build %s", c, childRun)
	}
	if err := b.e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	child, _ := b.s.GetTicket(ctx, r.TicketID)
	if child.Status != store.TicketReview || child.BaseSHA == nil || *child.BaseSHA != *parent.HeadSHA {
		run, _ := b.s.GetRun(ctx, c.ID)
		t.Fatalf("child = %+v (error %v), run error %v, want it based on %s", child, child.Error, run.Error, *parent.HeadSHA)
	}
	b.gh.Mu.Lock()
	defer b.gh.Mu.Unlock()
	if last := b.gh.Created[len(b.gh.Created)-1]; last["base"] != *parent.Branch || !strings.Contains(last["body"].(string), "Stacked on #7") {
		t.Fatalf("the stacked PR = %v", last)
	}
}

// Each lane of a race builds on its own branch with its variant, and the
// ticket goes to review once both delivered, with each lane's diff kept for
// the comparison.
func TestARaceBuildsEachLaneWithItsVariant(t *testing.T) {
	b := newBuildEnv(t)
	sha := b.e.Current().Config.Agents["orbit"].SHA
	r, _, err := b.s.CreateTicket(ctx, store.NewTicket{Kind: "request", Title: "Add hello", Body: "please", Agent: "orbit",
		SpecSHA: sha, Repo: "kensac/orbit", Author: "kanishk", Race: 2, Variants: []store.Variant{{Model: "opus", Effort: "high"}}})
	if err != nil {
		t.Fatal(err)
	}
	script, _ := json.Marshal(map[string]any{"steps": []any{writeStep("/work/hello.txt", "hi\n"), writeStep("/work/hello_test.go", "package x\n")}})
	_, _ = b.s.ProposePlan(ctx, r.TicketID, "", "Add hello.txt.\n\nFAKE-SCRIPT: "+string(script))
	_, _ = b.s.ApprovePlan(ctx, r.TicketID, 1, "kanishk", "")
	if _, err := b.s.StartRace(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "orbit", SpecSHA: sha, Purpose: "build"}); err != nil {
		t.Fatal(err)
	}
	argv := map[string]string{}
	for range 2 {
		c, _ := b.s.ClaimRun(ctx, "w")
		bd, _ := b.s.BuildOfRun(ctx, c.ID)
		if err := b.e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		at := b.sb.Attaches()
		argv[*bd.Lane] = strings.Join(at[len(at)-1].Argv, " ")
		run, _ := b.s.GetRun(ctx, c.ID)
		if want := map[string]string{"a": "opus", "b": "m"}[*bd.Lane]; run.Status != "succeeded" || run.Model == nil || *run.Model != want {
			t.Fatalf("lane %s run = %s model %v, want %s", *bd.Lane, run.Status, run.Model, want)
		}
	}
	if !strings.Contains(argv["a"], "--model opus") || !strings.Contains(argv["a"], "--effort high") ||
		!strings.Contains(argv["b"], "--model m") || strings.Contains(argv["b"], "--effort") {
		t.Fatalf("argv a = %s\nargv b = %s", argv["a"], argv["b"])
	}
	short := strings.ReplaceAll(r.TicketID, "-", "")[:8]
	for _, lane := range []string{"a", "b"} {
		if buildtest.RevParse(b.bare, "refs/heads/orbit/"+short+"-"+lane) == "" {
			t.Errorf("lane %s's branch was not pushed", lane)
		}
	}
	tk, _ := b.s.GetTicket(ctx, r.TicketID)
	if tk.Status != store.TicketReview || tk.PRNumber != nil {
		t.Fatalf("ticket = %+v", tk)
	}
	lanes, _ := b.s.RaceLanes(ctx, r.TicketID)
	for _, l := range lanes {
		if !l.Delivered || l.LinesChanged != 2 || l.TestsTouched != 1 || len(l.Files) != 2 || *l.DiffPolicy != "passed" ||
			*l.Branch != "orbit/"+short+"-"+l.Lane || l.CostUSD == nil {
			t.Errorf("lane = %+v", l)
		}
	}
}
