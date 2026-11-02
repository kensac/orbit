package tickets

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kensac/orbit/internal/github/githubtest"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

var ctx = context.Background()

const mergeSHA = "3333333333333333333333333333333333333333"

type env struct {
	t    *testing.T
	s    *store.Store
	gh   *githubtest.Server
	cfg  *spec.Config
	loop *Loop

	mu        sync.Mutex
	approvals []string
	wakes     int
}

// newEnv is a deployment that builds kensac/orbit with agent orbit, whose
// spec asks for automatic merges (which the self-hosting rule overrides).
func newEnv(t *testing.T, mergeRule string) *env {
	t.Helper()
	e := &env{t: t, s: testdb.New(t), gh: githubtest.New(t)}
	dir := t.TempDir()
	files := map[string]string{
		"orbit.yaml": `
defaults: {runner: claude, channels: [log]}
runners: {claude: {kind: claude-code, model: m}}
channels: {log: {kind: log}}
github:
  app_id: 42
  private_key_env: KEY
  api_url: ` + e.gh.URL() + `
  repositories: [kensac/orbit]
build:
  sandboxd: 127.0.0.1:1
  agent: orbit
  author: "orbit-kensac[bot] <351241810+orbit-kensac[bot]@users.noreply.github.com>"
  repositories:
    kensac/orbit: {commands: [go test]}
`,
		"agents/orbit.yaml": "name: orbit\ncharter: build orbit\ntimeout: 45m\nactions: {github:merge: " + mergeRule + "}\n",
	}
	for name, body := range files {
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
	e.cfg = cfg
	specs := map[string]store.AgentSpec{}
	for n, a := range cfg.Agents {
		b, _ := json.Marshal(a)
		specs[n] = store.AgentSpec{SHA: a.SHA, JSON: b, Enabled: true}
	}
	if err := e.s.SyncAgents(ctx, specs); err != nil {
		t.Fatal(err)
	}
	e.gh.Mu.Lock()
	e.gh.MergeSHA = mergeSHA
	e.gh.PR["head"].(map[string]any)["ref"] = "orbit/3f2a9c1e"
	e.gh.Mu.Unlock()
	e.loop = &Loop{
		Store: e.s, Config: func() *spec.Config { return e.cfg },
		Env: func(k string) (string, bool) {
			if k == "KEY" {
				return githubtest.KeyPEM(), true
			}
			return "", false
		},
		Client: e.gh.Srv.Client(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Approval: func(_ context.Context, agent, runID, actionID, title, body string) {
			e.mu.Lock()
			e.approvals = append(e.approvals, title+"\n"+body)
			e.mu.Unlock()
		},
		Wake: func() { e.mu.Lock(); e.wakes++; e.mu.Unlock() },
	}
	return e
}

func (e *env) tick() {
	e.t.Helper()
	if err := e.loop.Tick(ctx); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) ticket(id string) *store.Ticket {
	e.t.Helper()
	tk, err := e.s.GetTicket(ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tk
}

// approvedTicket files a request for kensac/orbit and approves its plan.
func (e *env) approvedTicket() string {
	e.t.Helper()
	a := e.cfg.Agents["orbit"]
	r, _, err := e.s.CreateTicket(ctx, store.NewTicket{Kind: "request", Title: "Add a flag", Body: "please", Agent: "orbit",
		SpecSHA: a.SHA, Repo: "kensac/orbit", Author: "kanishk"})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.ProposePlan(ctx, r.TicketID, "", "the plan"); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.ApprovePlan(ctx, r.TicketID, 1, "kanishk", ""); err != nil {
		e.t.Fatal(err)
	}
	// The ticket's first message woke nothing (Wake false); drain any run.
	return r.TicketID
}

// finishBuild plays the build run: claimed and delivered PR 7 at head.
func (e *env) finishBuild(ticketID, head string) string {
	e.t.Helper()
	b, err := e.s.LatestBuild(ctx, ticketID)
	if err != nil || b == nil || b.RunStatus != "queued" {
		e.t.Fatalf("latest build = %+v %v", b, err)
	}
	c, err := e.s.ClaimRun(ctx, "w")
	if err != nil || c == nil || c.ID != b.RunID {
		e.t.Fatalf("claim = %+v %v", c, err)
	}
	pr := 7
	if err := e.s.FinishBuild(ctx, c.ID, store.RunResult{Attempt: c.Attempt, Summary: "built"},
		store.TicketUpdate{BaseSHA: "base", Branch: "orbit/3f2a9c1e", PRNumber: &pr, HeadSHA: head}); err != nil {
		e.t.Fatal(err)
	}
	return c.ID
}

func TestMergeRule(t *testing.T) {
	for _, c := range []struct {
		repo string
		in   spec.Rule
		want spec.Rule
	}{
		{"kensac/orbit", spec.RuleAuto, spec.RuleApprove},
		{"kensac/orbit", spec.RuleApprove, spec.RuleApprove},
		{"kensac/orbit", spec.RuleDeny, spec.RuleDeny},
		{"kensac/other", spec.RuleAuto, spec.RuleAuto},
		{"kensac/other", spec.RuleDeny, spec.RuleDeny},
	} {
		if got := MergeRule(c.repo, c.in); got != c.want {
			t.Errorf("MergeRule(%s, %s) = %s, want %s", c.repo, c.in, got, c.want)
		}
	}
}

func TestATicketGoesFromApprovedToDone(t *testing.T) {
	e := newEnv(t, "auto")
	id := e.approvedTicket()
	e.tick()
	if st := e.ticket(id).Status; st != store.TicketBuilding {
		t.Fatalf("after approval: %s", st)
	}
	e.tick() // a queued build is left alone
	e.finishBuild(id, githubtest.Head)
	if st := e.ticket(id).Status; st != store.TicketReview {
		t.Fatalf("after the build: %s", st)
	}
	// Checks still running: wait.
	e.gh.Mu.Lock()
	e.gh.CheckRuns[0]["status"], e.gh.CheckRuns[0]["conclusion"] = "in_progress", nil
	e.gh.CompareFiles = []string{"internal/github/merge.go", "README.md"}
	e.gh.Mu.Unlock()
	e.tick()
	if st := e.ticket(id).Status; st != store.TicketReview {
		t.Fatalf("with checks running: %s", st)
	}
	e.gh.Mu.Lock()
	e.gh.CheckRuns[0]["status"], e.gh.CheckRuns[0]["conclusion"] = "completed", "success"
	e.gh.Mu.Unlock()
	e.tick()
	tk := e.ticket(id)
	if tk.Status != store.TicketMerging || tk.MergeActionID == nil {
		t.Fatalf("after green checks: %+v", tk)
	}
	// orbit's own repository: approve, whatever the spec says.
	act, _ := e.s.GetAction(ctx, *tk.MergeActionID)
	if act.Rule != "approve" || act.Status != "pending" {
		t.Fatalf("merge action = %+v; orbit's own merges always wait for a person", act)
	}
	var p struct {
		Repo       string `json:"repo"`
		PullNumber int    `json:"pull_number"`
		HeadSHA    string `json:"head_sha"`
	}
	_ = json.Unmarshal(act.Params, &p)
	if p.Repo != "kensac/orbit" || p.PullNumber != 7 || p.HeadSHA != githubtest.Head {
		t.Fatalf("params = %s", act.Params)
	}
	if len(e.approvals) != 1 || !strings.Contains(e.approvals[0], "Trust boundary") || !strings.Contains(e.approvals[0], "internal/github/merge.go") {
		t.Fatalf("approvals = %q", e.approvals)
	}
	e.tick() // pending: nothing changes
	if st := e.ticket(id).Status; st != store.TicketMerging {
		t.Fatal(st)
	}
	// The person approves; the action executes (as engine.ExecuteActions
	// does) and records the merge commit.
	if err := e.s.DecideAction(ctx, act.ID, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	claimed, _ := e.s.ClaimApprovedAction(ctx)
	if err := e.s.FinishAction(ctx, claimed.ID, json.RawMessage(`{"merged":true,"merge_sha":"`+mergeSHA+`"}`), nil); err != nil {
		t.Fatal(err)
	}
	e.loop.Revision = "4444444444444444444444444444444444444444"
	e.gh.Mu.Lock()
	e.gh.Compare = map[string]string{mergeSHA + "..." + e.loop.Revision: "behind"}
	e.gh.Mu.Unlock()
	e.tick()
	tk = e.ticket(id)
	if tk.Status != store.TicketMerged || *tk.MergeSHA != mergeSHA {
		t.Fatalf("after the merge: %+v", tk)
	}
	// The running orbit does not include the merge yet: merged, not done.
	e.tick()
	if st := e.ticket(id).Status; st != store.TicketMerged {
		t.Fatalf("before the deploy: %s", st)
	}
	// The deploy: orbit now runs a descendant of the merge commit.
	e.gh.Mu.Lock()
	e.gh.Compare = map[string]string{mergeSHA + "..." + e.loop.Revision: "ahead"}
	e.gh.Mu.Unlock()
	e.tick()
	if st := e.ticket(id).Status; st != store.TicketDone {
		t.Fatalf("after the deploy: %s", st)
	}
}

func TestDoneWhenOrbitRunsTheMergeCommit(t *testing.T) {
	e := newEnv(t, "approve")
	id := e.approvedTicket()
	e.tick()
	e.finishBuild(id, githubtest.Head)
	e.loop.Revision = mergeSHA
	// A person merged the PR on GitHub while it was in review.
	e.gh.Mu.Lock()
	e.gh.PR["merged"], e.gh.PR["state"], e.gh.PR["merge_commit_sha"] = true, "closed", mergeSHA
	e.gh.Mu.Unlock()
	e.tick()
	if st := e.ticket(id).Status; st != store.TicketDone {
		t.Fatalf("status = %s", st)
	}
}

func TestFailingChecksGetBoundedFixAttempts(t *testing.T) {
	e := newEnv(t, "approve")
	id := e.approvedTicket()
	e.tick()
	e.finishBuild(id, githubtest.Head)
	e.gh.Mu.Lock()
	e.gh.CheckRuns[0]["conclusion"], e.gh.CheckRuns[0]["id"], e.gh.CheckRuns[0]["html_url"] = "failure", 991, "https://github.com/x/runs/991"
	e.gh.CheckRuns[0]["app"] = map[string]any{"id": 15368, "slug": "github-actions"}
	e.gh.JobLogs = map[int64]string{991: strings.Repeat("noise\n", 5000) + "--- FAIL: TestThing\n"}
	e.gh.Mu.Unlock()
	for attempt := 1; attempt <= MaxFixAttempts; attempt++ {
		e.tick()
		tk := e.ticket(id)
		if tk.Status != store.TicketBuilding {
			t.Fatalf("fix %d: status %s", attempt, tk.Status)
		}
		b, _ := e.s.LatestBuild(ctx, id)
		if b.Purpose != "fix" {
			t.Fatalf("fix %d: purpose %s", attempt, b.Purpose)
		}
		var report []map[string]any
		_ = json.Unmarshal(b.Failure, &report)
		tail, _ := report[0]["log_tail"].(string)
		if len(report) != 1 || report[0]["name"] != "check" || !strings.HasSuffix(tail, "--- FAIL: TestThing\n") || len(tail) > logTailBytes {
			t.Fatalf("report = %v", report)
		}
		e.finishBuild(id, githubtest.Head)
	}
	e.tick()
	tk := e.ticket(id)
	if tk.Status != store.TicketFailed || !strings.Contains(*tk.Error, "check (failure) https://github.com/x/runs/991") {
		t.Fatalf("after %d fixes: %+v", MaxFixAttempts, tk)
	}
}

func TestOneBuildAtATimePerRepository(t *testing.T) {
	e := newEnv(t, "approve")
	one, two := e.approvedTicket(), e.approvedTicket()
	e.tick()
	if a, b := e.ticket(one).Status, e.ticket(two).Status; a != store.TicketBuilding || b != store.TicketApproved {
		t.Fatalf("statuses %s %s", a, b)
	}
	e.finishBuild(one, githubtest.Head)
	e.tick()
	if st := e.ticket(two).Status; st != store.TicketBuilding {
		t.Fatalf("the second build after the first: %s", st)
	}
}

// Every step a crash can separate from its row heals from GitHub's state.
func TestReconciliation(t *testing.T) {
	t.Run("build run lost by the reaper", func(t *testing.T) {
		e := newEnv(t, "approve")
		id := e.approvedTicket()
		e.tick()
		b, _ := e.s.LatestBuild(ctx, id)
		c, _ := e.s.ClaimRun(ctx, "w")
		// The worker died for the last time: the reaper fails the run, not
		// the ticket.
		if err := e.s.FinishRun(ctx, c.ID, store.RunResult{Attempt: c.Attempt, Status: "failed", Error: "worker lost on attempt 3 of 3"}); err != nil {
			t.Fatal(err)
		}
		if b.RunID != c.ID || e.ticket(id).Status != store.TicketBuilding {
			t.Fatal("setup")
		}
		e.tick()
		if tk := e.ticket(id); tk.Status != store.TicketFailed || !strings.Contains(*tk.Error, "worker lost") {
			t.Fatalf("%+v", tk)
		}
	})
	t.Run("merge happened, its action failed as orphaned", func(t *testing.T) {
		e := newEnv(t, "approve")
		id := e.approvedTicket()
		e.tick()
		e.finishBuild(id, githubtest.Head)
		e.tick()
		tk := e.ticket(id)
		_ = e.s.DecideAction(ctx, *tk.MergeActionID, true, "kanishk")
		a, _ := e.s.ClaimApprovedAction(ctx)
		// The merge reached GitHub; the process died before FinishAction,
		// and the next start failed the action as orphaned.
		e.gh.Mu.Lock()
		e.gh.PR["merged"], e.gh.PR["state"], e.gh.PR["merge_commit_sha"] = true, "closed", mergeSHA
		e.gh.Mu.Unlock()
		_ = e.s.FinishAction(ctx, a.ID, nil, errOrphan)
		e.tick()
		if tk := e.ticket(id); tk.Status != store.TicketMerged || *tk.MergeSHA != mergeSHA {
			t.Fatalf("%+v", tk)
		}
	})
	t.Run("merge failed, then GitHub merged it", func(t *testing.T) {
		e := newEnv(t, "approve")
		id := e.approvedTicket()
		e.tick()
		e.finishBuild(id, githubtest.Head)
		e.tick()
		tk := e.ticket(id)
		_ = e.s.DecideAction(ctx, *tk.MergeActionID, true, "kanishk")
		a, _ := e.s.ClaimApprovedAction(ctx)
		_ = e.s.FinishAction(ctx, a.ID, nil, errOrphan)
		e.tick()
		if tk := e.ticket(id); tk.Status != store.TicketFailed {
			t.Fatalf("a failed merge with the PR open: %s", tk.Status)
		}
		e.gh.Mu.Lock()
		e.gh.PR["merged"], e.gh.PR["state"], e.gh.PR["merge_commit_sha"] = true, "closed", mergeSHA
		e.gh.Mu.Unlock()
		e.tick()
		if tk := e.ticket(id); tk.Status != store.TicketMerged {
			t.Fatalf("%+v", tk)
		}
	})
	t.Run("rejected merge", func(t *testing.T) {
		e := newEnv(t, "approve")
		id := e.approvedTicket()
		e.tick()
		e.finishBuild(id, githubtest.Head)
		e.tick()
		_ = e.s.DecideAction(ctx, *e.ticket(id).MergeActionID, false, "kanishk")
		e.tick()
		if tk := e.ticket(id); tk.Status != store.TicketFailed || !strings.Contains(*tk.Error, "rejected by kanishk") {
			t.Fatalf("%+v", tk)
		}
	})
	t.Run("head moved after the build", func(t *testing.T) {
		e := newEnv(t, "approve")
		id := e.approvedTicket()
		e.tick()
		e.finishBuild(id, "5555555555555555555555555555555555555555")
		e.tick()
		if tk := e.ticket(id); tk.Status != store.TicketFailed || !strings.Contains(*tk.Error, "head moved") {
			t.Fatalf("%+v", tk)
		}
	})
	t.Run("PR closed", func(t *testing.T) {
		e := newEnv(t, "approve")
		id := e.approvedTicket()
		e.tick()
		e.finishBuild(id, githubtest.Head)
		e.gh.Mu.Lock()
		e.gh.PR["state"] = "closed"
		e.gh.Mu.Unlock()
		e.tick()
		if tk := e.ticket(id); tk.Status != store.TicketFailed {
			t.Fatalf("%+v", tk)
		}
	})
	t.Run("abandoned with an open PR", func(t *testing.T) {
		e := newEnv(t, "approve")
		id := e.approvedTicket()
		e.tick()
		e.finishBuild(id, githubtest.Head)
		if err := e.s.TransitionTicket(ctx, id, store.TicketAbandoned, "kanishk", store.TicketUpdate{}); err != nil {
			t.Fatal(err)
		}
		e.tick()
		e.gh.Mu.Lock()
		defer e.gh.Mu.Unlock()
		if e.gh.PR["state"] != "closed" {
			t.Fatalf("PR = %v", e.gh.PR["state"])
		}
	})
	t.Run("spec denies merges", func(t *testing.T) {
		e := newEnv(t, "deny")
		id := e.approvedTicket()
		e.tick()
		e.finishBuild(id, githubtest.Head)
		e.tick()
		if tk := e.ticket(id); tk.Status != store.TicketReview || tk.MergeActionID != nil {
			t.Fatalf("%+v", tk)
		}
	})
}

type orphanErr struct{}

func (orphanErr) Error() string {
	return "interrupted during execution; the side effect may or may not have happened"
}

var errOrphan = orphanErr{}
