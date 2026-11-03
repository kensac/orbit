package engine_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/build/buildtest"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/github/githubtest"
	orbitharness "github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/repos"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

const repoOutput = `{"summary": "read it", "report": "Read it.", "notify": {"send": false, "priority": "default", "title": ""},
  "memory": {"update": false, "content": ""}, "questions": [], "actions": []}`

// repoWorld is an engine whose agent reader reads kensac/orbit, a real
// local git remote standing in for GitHub's, and the fake GitHub API.
type repoWorld struct {
	e    *engine.Engine
	s    *store.Store
	gh   *githubtest.Server
	bare string
	seed string // a working clone that pushes to bare
}

func newRepoWorld(t *testing.T, fr *fakeRunner, agentExtra string) *repoWorld {
	t.Helper()
	w := &repoWorld{s: testdb.New(t), gh: githubtest.New(t), bare: buildtest.Repo(t)}
	w.seed = filepath.Join(t.TempDir(), "seed")
	buildtest.Git(t, filepath.Dir(w.seed), "clone", "--quiet", w.bare, w.seed)
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("orbit.yaml", fmt.Sprintf(`
timezone: America/New_York
defaults: {runner: fake, channels: [log]}
runners: {fake: {kind: claude-code}}
channels: {log: {kind: log}}
github:
  app_id: %d
  private_key_env: GH_KEY
  api_url: %s
  repositories: [kensac/elsewhere]
  read_repositories: [kensac/orbit]
`, githubtest.AppID, w.gh.URL()))
	write("agents/reader.yaml", `
name: reader
charter: Know the code.
triggers: [{schedule: "0 9 * * *"}]
needs: [github]
repos: [{repo: kensac/orbit, ref: main}]
`+agentExtra)
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	env := func(k string) (string, bool) {
		if k == "GH_KEY" {
			return githubtest.KeyPEM(), true
		}
		return "", false
	}
	w.e = &engine.Engine{
		Store: w.s, DataDir: t.TempDir(), ConfigDir: dir, Env: env, Client: w.gh.Srv.Client(),
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) { return fr, nil },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		// Any path: the fake runner never starts it, and it makes the
		// engine offer orbit's tools.
		Harness: "/nonexistent/orbit",
		Repos:   &repos.Mirrors{Root: filepath.Join(t.TempDir(), "mirrors"), URL: func(string) string { return w.bare }},
	}
	if err := w.e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	specs := map[string]store.AgentSpec{}
	for n, a := range cfg.Agents {
		b, _ := json.Marshal(a)
		specs[n] = store.AgentSpec{SHA: a.SHA, JSON: b, Enabled: true}
	}
	if err := w.s.SyncAgents(ctx, specs); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *repoWorld) run(t *testing.T, trigger string, minute int) *store.ClaimedRun {
	t.Helper()
	a := w.e.Current().Config.Agents["reader"]
	nr := store.NewRun{Agent: "reader", SpecSHA: a.SHA, Trigger: trigger, Prompt: "look"}
	if trigger == "schedule" {
		idx, at := 0, fireBase.Add(time.Duration(minute)*time.Minute)
		nr.TriggerIndex, nr.ScheduledFor, nr.Prompt = &idx, &at, ""
	}
	if _, err := w.s.EnqueueRun(ctx, nr); err != nil {
		t.Fatal(err)
	}
	c, err := w.s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := w.e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func (w *repoWorld) push(t *testing.T, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(w.seed, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	buildtest.Git(t, w.seed, "add", ".")
	buildtest.Git(t, w.seed, "commit", "--quiet", "-m", "change "+name)
	buildtest.Git(t, w.seed, "push", "--quiet", "origin", "HEAD:main")
	return buildtest.RevParse(w.bare, "refs/heads/main")
}

// TestAnAgentReadsARepositoryAtACommit: a run is given the tree at main's
// head without .git, named in its prompt, recorded as _repos.json; the
// fetch's token is contents:read and revoked; the repository tools answer
// over the socket from the mirror and GitHub; and a scheduled run whose
// commit has not moved is skipped by unchanged, while a new commit runs.
func TestAnAgentReadsARepositoryAtACommit(t *testing.T) {
	type seen struct {
		files  []string
		prompt string
		tools  map[string]string
		errs   map[string]string
		repos  string
	}
	var got []seen
	fr := &fakeRunner{output: repoOutput}
	w := newRepoWorld(t, fr, "gates: [{kind: unchanged}]\n")
	w.gh.Mu.Lock()
	w.gh.Others = map[int]map[string]any{
		5: {"number": 5, "state": "closed", "merged_at": "2026-10-30T12:00:00Z", "title": "merged one",
			"head": map[string]any{"ref": "orbit/aaaa", "sha": githubtest.Head}, "base": map[string]any{"ref": "main"}},
	}
	w.gh.Mu.Unlock()
	fr.seen = func(req runner.Request) {
		var sn seen
		sn.prompt = req.Prompt
		root := filepath.Join(req.Workdir, "inputs", "repos", "orbit")
		_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				rel, _ := filepath.Rel(root, p)
				sn.files = append(sn.files, filepath.ToSlash(rel))
			}
			return nil
		})
		slices.Sort(sn.files)
		if req.Harness != nil {
			sn.repos = req.Harness.Repos
		}
		sn.tools, sn.errs = map[string]string{}, map[string]string{}
		for name, args := range map[string]string{
			"git_log":   `{"max": 5}`,
			"git_show":  `{"rev": "main", "path": "README.md"}`,
			"pr_list":   `{"state": "all"}`,
			"pr_view":   `{"number": 7}`,
			"bad_repo":  `{"repo": "kensac/elsewhere"}`,
			"bad_field": `{"cmd": "rm -rf /"}`,
		} {
			tool := name
			if strings.HasPrefix(name, "bad_") {
				tool = "git_log"
			}
			text, err := orbitharness.Repo(sn.repos, orbitharness.RepoRequest{Tool: tool, Args: json.RawMessage(args)})
			if err != nil {
				sn.errs[name] = err.Error()
			} else {
				sn.tools[name] = text
			}
		}
		got = append(got, sn)
	}

	main := buildtest.RevParse(w.bare, "refs/heads/main")
	first := w.run(t, "schedule", 0)
	if r, _ := w.s.GetRun(ctx, first.ID); r.Status != "succeeded" || len(got) != 1 {
		t.Fatalf("first run %s, %d model calls: %v", r.Status, len(got), r.Error)
	}
	g := got[0]
	if want := []string{".github/workflows/ci.yml", "README.md", "deploy/Dockerfile", "go.mod", "main.go"}; !slices.Equal(g.files, want) {
		t.Fatalf("the run's tree = %v, want %v (no .git)", g.files, want)
	}
	if !strings.Contains(g.prompt, "kensac/orbit at main, commit "+main) || !strings.Contains(g.prompt, "`inputs/repos/orbit/`") || !strings.Contains(g.prompt, "`git_log`") {
		t.Fatalf("the prompt does not name the tree and its commit:\n%s", g.prompt)
	}
	arts, err := w.s.AttemptArtifacts(ctx, first.ID, first.Attempt, "in")
	if err != nil {
		t.Fatal(err)
	}
	var record []map[string]string
	for _, a := range arts {
		if a.Name == "inputs/_repos.json" {
			_ = json.Unmarshal(a.Content, &record)
		}
		if strings.Contains(a.Name, "repos/orbit/") {
			t.Fatalf("a tree file was stored as an artifact: %s", a.Name)
		}
	}
	if len(record) != 1 || record[0]["repo"] != "kensac/orbit" || record[0]["ref"] != "main" || record[0]["sha"] != main {
		t.Fatalf("_repos.json = %v", record)
	}

	// The tools: exact history from the mirror, and GitHub's state.
	var log []repos.LogEntry
	if err := json.Unmarshal([]byte(g.tools["git_log"]), &log); err != nil || len(log) != 1 || log[0].SHA != main {
		t.Fatalf("git_log = %q %v", g.tools["git_log"], err)
	}
	if g.tools["git_show"] != "# x\n" {
		t.Fatalf("git_show = %q", g.tools["git_show"])
	}
	if !strings.Contains(g.tools["pr_list"], `"number":7`) || !strings.Contains(g.tools["pr_list"], `"merged_at":"2026-10-30T12:00:00Z"`) {
		t.Fatalf("pr_list = %s", g.tools["pr_list"])
	}
	if !strings.Contains(g.tools["pr_view"], `{"conclusion":"success","name":"check","status":"completed"`) || !strings.Contains(g.tools["pr_view"], `"title":"Add a thing"`) {
		t.Fatalf("pr_view = %s", g.tools["pr_view"])
	}
	if !strings.Contains(g.errs["bad_repo"], "not one of yours") || !strings.Contains(g.errs["bad_field"], "unknown field") {
		t.Fatalf("refusals = %v", g.errs)
	}
	// Every token was a read token for kensac/orbit, and each was revoked.
	w.gh.Mu.Lock()
	revoked, calls := len(w.gh.Revoked), slices.Clone(w.gh.Calls)
	w.gh.Mu.Unlock()
	if mints := strings.Count(strings.Join(calls, "\n"), "access_tokens"); mints == 0 || mints != revoked {
		t.Fatalf("%d tokens minted, %d revoked", mints, revoked)
	}

	// Nothing moved: the scheduled run is skipped without the model.
	second := w.run(t, "schedule", 1)
	if r, _ := w.s.GetRun(ctx, second.ID); r.Status != store.StatusSkipped || len(got) != 1 {
		t.Fatalf("an unchanged commit: run %s, %d model calls", r.Status, len(got))
	}
	// A new commit on main is a change.
	next := w.push(t, "NEW.md", "new\n")
	third := w.run(t, "schedule", 2)
	if r, _ := w.s.GetRun(ctx, third.ID); r.Status != "succeeded" || len(got) != 2 {
		t.Fatalf("a new commit: run %s, %d model calls", r.Status, len(got))
	}
	if !slices.Contains(got[1].files, "NEW.md") || !strings.Contains(got[1].prompt, next) {
		t.Fatalf("the new commit's tree = %v", got[1].files)
	}
}

// TestTheMintedTokenReadsOnly: the fetch asks GitHub for contents:read and
// nothing else, and pr tools for pull request, check and status reads.
func TestTheMintedTokenReadsOnly(t *testing.T) {
	var perms []map[string]any
	fr := &fakeRunner{output: repoOutput}
	w := newRepoWorld(t, fr, "")
	fr.seen = func(req runner.Request) {
		w.gh.Mu.Lock()
		perms = append(perms, w.gh.TokenReq["permissions"].(map[string]any))
		w.gh.Mu.Unlock()
		if _, err := orbitharness.Repo(req.Harness.Repos, orbitharness.RepoRequest{Tool: "pr_list", Args: json.RawMessage(`{}`)}); err != nil {
			t.Error(err)
		}
		w.gh.Mu.Lock()
		perms = append(perms, w.gh.TokenReq["permissions"].(map[string]any))
		w.gh.Mu.Unlock()
	}
	w.run(t, "manual", 0)
	if len(perms) != 2 || fmt.Sprint(perms[0]) != "map[contents:read]" ||
		fmt.Sprint(perms[1]) != "map[checks:read pull_requests:read statuses:read]" {
		t.Fatalf("permissions asked = %v", perms)
	}
}

// TestAChatAgentGetsTheRepositoryTools: a chat run has the tools among its
// own, answered in orbit's process.
func TestAChatAgentGetsTheRepositoryTools(t *testing.T) {
	var names []string
	var answer string
	fr := &fakeRunner{output: repoOutput}
	w := newRepoWorld(t, fr, "chat: true\n")
	fr.seen = func(req runner.Request) {
		for _, tl := range req.Chat.Tools {
			names = append(names, tl.Name)
		}
		answer, _ = req.Chat.Call(ctx, "git_blame", json.RawMessage(`{"path": "go.mod"}`))
	}
	w.run(t, "manual", 0)
	for _, want := range []string{"git_log", "git_show", "git_diff", "git_blame", "pr_list", "pr_view", "remember"} {
		if !slices.Contains(names, want) {
			t.Fatalf("chat tools %v lack %s", names, want)
		}
	}
	if !strings.Contains(answer, "module example.com/x") {
		t.Fatalf("git_blame = %q", answer)
	}
}
