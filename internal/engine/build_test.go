package engine_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
	env := map[string]string{"KEY": githubtest.KeyPEM(), "ORBIT_SANDBOX_TOKEN": buildtest.SandboxToken,
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-engine-test-credential-000000"}
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
	if _, err := b.s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "orbit", SpecSHA: sha, Purpose: "build"}); err != nil {
		t.Fatal(err)
	}
	c, _ = b.s.ClaimRun(ctx, "w")
	if err := b.e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	child, _ := b.s.GetTicket(ctx, r.TicketID)
	if child.Status != store.TicketReview || child.BaseSHA == nil || *child.BaseSHA != *parent.HeadSHA {
		t.Fatalf("child = %+v, want it based on %s", child, *parent.HeadSHA)
	}
	b.gh.Mu.Lock()
	defer b.gh.Mu.Unlock()
	if last := b.gh.Created[len(b.gh.Created)-1]; last["base"] != *parent.Branch || !strings.Contains(last["body"].(string), "Stacked on #7") {
		t.Fatalf("the stacked PR = %v", last)
	}
}
