package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kensac/orbit/internal/build"
	"github.com/kensac/orbit/internal/github"
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

// failCheck makes the required check fail as an Actions job, 991.
func (e *env) failCheck() {
	e.gh.Mu.Lock()
	defer e.gh.Mu.Unlock()
	e.gh.CheckRuns[0]["status"], e.gh.CheckRuns[0]["conclusion"] = "completed", "failure"
	e.gh.CheckRuns[0]["id"], e.gh.CheckRuns[0]["html_url"] = 991, "https://github.com/x/runs/991"
	e.gh.CheckRuns[0]["app"] = map[string]any{"id": 15368, "slug": "github-actions"}
	e.gh.JobLogs = map[int64]string{991: strings.Repeat("noise\n", 5000) + "2026-11-01T10:00:00.1234567Z \x1b[31m--- FAIL: TestThing\x1b[0m\n"}
}

func TestFailingChecksAreRerunOnceThenGetBoundedFixAttempts(t *testing.T) {
	e := newEnv(t, "approve")
	id := e.approvedTicket()
	e.tick()
	e.finishBuild(id, githubtest.Head)
	e.failCheck()
	// The first failure on a head is rerun, not fixed: a flaky test costs
	// a rerun, not a build.
	e.tick()
	if st := e.ticket(id).Status; st != store.TicketReview {
		t.Fatalf("after the first failure: %s", st)
	}
	e.gh.Mu.Lock()
	reruns, status := e.gh.Reruns, e.gh.CheckRuns[0]["status"]
	e.gh.Mu.Unlock()
	if len(reruns) != 1 || reruns[0] != 991 || status != "queued" {
		t.Fatalf("reruns = %v, check %v", reruns, status)
	}
	e.tick() // the rerun is queued: wait
	if st := e.ticket(id).Status; st != store.TicketReview {
		t.Fatalf("while the rerun runs: %s", st)
	}
	for attempt := 1; attempt <= MaxFixAttempts; attempt++ {
		e.failCheck()
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
		if len(report) != 1 || report[0]["name"] != "check" || report[0]["on_base"] != "absent" ||
			!strings.HasSuffix(tail, "noise\n--- FAIL: TestThing\n") || strings.Count(tail, "\n") != logTailLines {
			t.Fatalf("report = %v", report)
		}
		e.finishBuild(id, githubtest.Head)
	}
	e.failCheck()
	e.tick()
	tk := e.ticket(id)
	if tk.Status != store.TicketFailed || !strings.Contains(*tk.Error, "check (failure) https://github.com/x/runs/991") {
		t.Fatalf("after %d fixes: %+v", MaxFixAttempts, tk)
	}
	if e.gh.Reruns[0] != 991 || len(e.gh.Reruns) != 1 {
		t.Fatalf("a head is rerun once by the loop: %v", e.gh.Reruns)
	}
}

func TestARefusedRerunFallsBackToAFix(t *testing.T) {
	e := newEnv(t, "approve")
	id := e.approvedTicket()
	e.tick()
	e.finishBuild(id, githubtest.Head)
	e.failCheck()
	e.gh.Mu.Lock()
	e.gh.RerunStatus = 403
	e.gh.Mu.Unlock()
	e.tick()
	if st := e.ticket(id).Status; st != store.TicketBuilding {
		t.Fatalf("a refused rerun: %s", st)
	}
	rr, _ := e.s.TicketReruns(ctx, id)
	if len(rr) != 1 || rr[0].Error == nil || !strings.Contains(*rr[0].Error, "Resource not accessible") || rr[0].RequestedBy != "orbit" {
		t.Fatalf("reruns = %+v", rr)
	}
}

func TestAFailureTheBaseSharesSpendsNoFix(t *testing.T) {
	e := newEnv(t, "approve")
	id := e.approvedTicket()
	e.tick()
	e.finishBuild(id, githubtest.Head)
	e.gh.Mu.Lock()
	e.gh.CommitChecks = map[string][]map[string]any{"base": {{"name": "check", "status": "completed", "conclusion": "failure"}}}
	e.gh.Mu.Unlock()
	e.failCheck()
	e.tick() // rerun
	e.failCheck()
	e.tick()
	tk := e.ticket(id)
	if tk.Status != store.TicketFailed || !strings.Contains(*tk.Error, "also fails on base") {
		t.Fatalf("%+v", tk)
	}
	if b, _ := e.s.LatestBuild(ctx, id); b.Purpose != "build" {
		t.Fatalf("a fix was spent on a failure the base has: %s", b.Purpose)
	}
}

func TestAPersonRerunsTheChecksOfAFailedTicket(t *testing.T) {
	e := newEnv(t, "approve")
	id := e.approvedTicket()
	e.tick()
	e.finishBuild(id, githubtest.Head)
	if err := e.s.TransitionTicket(ctx, id, store.TicketFailed, "orbit", store.TicketUpdate{Error: "checks failed"}); err != nil {
		t.Fatal(err)
	}
	e.failCheck()
	if _, err := e.s.RequestRerun(ctx, id, "kanishk"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RequestRerun(ctx, id, "kanishk"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second request while one waits = %v", err)
	}
	e.tick()
	tk := e.ticket(id)
	if tk.Status != store.TicketReview {
		t.Fatalf("after the rerun: %+v", tk)
	}
	rr, _ := e.s.TicketReruns(ctx, id)
	if len(rr) != 1 || rr[0].DoneAt == nil || rr[0].Error != nil || !strings.Contains(string(rr[0].Jobs), `"job_id": 991`) {
		t.Fatalf("reruns = %+v", rr)
	}
	if e.gh.Reruns[0] != 991 {
		t.Fatalf("GitHub reruns = %v", e.gh.Reruns)
	}

	// A request whose head moved on is answered with why, and the ticket
	// stays where it is.
	_ = e.s.TransitionTicket(ctx, id, store.TicketFailed, "orbit", store.TicketUpdate{Error: "checks failed"})
	if _, err := e.s.RequestRerun(ctx, id, "kanishk"); err != nil {
		t.Fatal(err)
	}
	e.gh.Mu.Lock()
	e.gh.PR["head"].(map[string]any)["sha"] = "5555555555555555555555555555555555555555"
	e.gh.Mu.Unlock()
	e.tick()
	rr, _ = e.s.TicketReruns(ctx, id)
	if tk := e.ticket(id); tk.Status != store.TicketFailed || len(rr) != 2 || rr[1].Error == nil || !strings.Contains(*rr[1].Error, "head is 555555555555") {
		t.Fatalf("ticket %s, reruns %+v", tk.Status, rr)
	}
}

func TestReruningNeedsAPR(t *testing.T) {
	e := newEnv(t, "approve")
	id := e.approvedTicket()
	if _, err := e.s.RequestRerun(ctx, id, "kanishk"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("an approved ticket = %v", err)
	}
	// failed -> review is a rerun's alone.
	e.tick()
	e.finishBuild(id, githubtest.Head)
	_ = e.s.TransitionTicket(ctx, id, store.TicketFailed, "orbit", store.TicketUpdate{Error: "x"})
	if err := e.s.TransitionTicket(ctx, id, store.TicketReview, "kanishk", store.TicketUpdate{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("failed -> review by hand = %v", err)
	}
}

func TestTidyLog(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"a\r\nb\n", "a\nb\n"},
		{"2026-11-01T10:00:00.1234567Z \x1b[1;31mFAIL\x1b[0m x\n", "FAIL x\n"},
		{"1\n2\n3\n4\n", "3\n4\n"},
		{"no newline", "no newline"},
	} {
		if got := TidyLog(c.in, 2); got != c.want {
			t.Errorf("TidyLog(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// A tail cut mid-line loses its partial first line.
	long := "partial" + strings.Repeat("x", logTailBytes) + "\nwhole\n"
	if got := TidyLog(long, 10); got != "whole\n" {
		t.Errorf("cut tail = %q", got)
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

func TestASubmittedReviewIsBuiltBeforeTheMerge(t *testing.T) {
	e := newEnv(t, "approve")
	id := e.approvedTicket()
	e.tick()
	e.finishBuild(id, githubtest.Head)
	if _, err := e.s.AddReviewComment(ctx, store.NewComment{TicketID: id, Path: "main.go", StartLine: 3, EndLine: 3, Body: "use a constant", Author: "kanishk"}); err != nil {
		t.Fatal(err)
	}
	// The checks are green, but a submitted review comes first: a revise,
	// not a merge.
	if _, _, err := e.s.SubmitReview(ctx, id, "kanishk", "", ""); err != nil {
		t.Fatal(err)
	}
	e.tick()
	tk := e.ticket(id)
	b, _ := e.s.LatestBuild(ctx, id)
	if tk.Status != store.TicketBuilding || tk.MergeActionID != nil || b.Purpose != store.BuildPurposeRevise {
		t.Fatalf("after the review: %+v, build %+v", tk, b)
	}
	const revised = "6666666666666666666666666666666666666666"
	e.gh.Mu.Lock()
	e.gh.PR["head"].(map[string]any)["sha"] = revised
	e.gh.Mu.Unlock()
	run := e.finishBuild(id, revised)
	cs, _ := e.s.ReviewComments(ctx, id)
	if len(cs) != 1 || cs[0].Status != store.CommentDelivered || *cs[0].DeliveredBy != run {
		t.Fatalf("comments = %+v", cs)
	}
	e.tick()
	if tk := e.ticket(id); tk.Status != store.TicketMerging {
		t.Fatalf("after the revise: %s", tk.Status)
	}
}

const childHead = "7777777777777777777777777777777777777777"

// stackedChild files a request stacked on parent and approves its plan.
func (e *env) stackedChild(parent string) string {
	e.t.Helper()
	a := e.cfg.Agents["orbit"]
	r, _, err := e.s.CreateTicket(ctx, store.NewTicket{Kind: "request", Title: "On top", Body: "after that", Agent: "orbit",
		SpecSHA: a.SHA, Repo: "kensac/orbit", Author: "kanishk", StackOn: parent})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.ProposePlan(ctx, r.TicketID, "", "the next plan"); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.s.ApprovePlan(ctx, r.TicketID, 1, "kanishk", ""); err != nil {
		e.t.Fatal(err)
	}
	return r.TicketID
}

// finishChild plays the stacked ticket's build: PR 8 on the parent's
// branch, its base the parent's head.
func (e *env) finishChild(id, base string) {
	e.t.Helper()
	b, _ := e.s.LatestBuild(ctx, id)
	c, err := e.s.ClaimRun(ctx, "w")
	if err != nil || c == nil || b == nil || c.ID != b.RunID {
		e.t.Fatalf("claim = %+v %v, build %+v", c, err, b)
	}
	pr := 8
	if err := e.s.FinishBuild(ctx, c.ID, store.RunResult{Attempt: c.Attempt, Summary: "built"},
		store.TicketUpdate{BaseSHA: base, Branch: "orbit/child", PRNumber: &pr, HeadSHA: childHead}); err != nil {
		e.t.Fatal(err)
	}
	e.gh.Mu.Lock()
	defer e.gh.Mu.Unlock()
	e.gh.Others = map[int]map[string]any{8: {
		"number": 8, "state": "open", "draft": false, "merged": false, "mergeable_state": "clean", "title": "On top",
		"head": map[string]any{"ref": "orbit/child", "sha": childHead, "repo": map[string]any{"full_name": "kensac/orbit"}},
		"base": map[string]any{"ref": "orbit/3f2a9c1e", "repo": map[string]any{"full_name": "kensac/orbit", "default_branch": "main"}},
	}}
	green := []map[string]any{{"name": "check", "status": "completed", "conclusion": "success", "app": map[string]any{"id": 15368}}}
	e.gh.CommitChecks = map[string][]map[string]any{childHead: green, "8888888888888888888888888888888888888888": green}
}

// restacks records the loop's restacks and plays them: the branch moves
// to 8888..., as a push would move PR 8's head.
func (e *env) restacks() *[]build.Restack {
	var got []build.Restack
	e.loop.Restack = func(_ context.Context, _ *github.App, r build.Restack) (*build.Restacked, error) {
		got = append(got, r)
		const head = "8888888888888888888888888888888888888888"
		e.gh.Mu.Lock()
		e.gh.Others[8]["head"].(map[string]any)["sha"] = head
		e.gh.Mu.Unlock()
		onto := r.Onto
		if onto == "" {
			onto = "main"
		}
		return &build.Restacked{Base: "9999999999999999999999999999999999999999", Head: head, Branch: onto}, nil
	}
	return &got
}

func TestAStackedTicketBuildsOnItsParentAndMergesAfterIt(t *testing.T) {
	e := newEnv(t, "approve")
	e.loop.Revision = mergeSHA
	got := e.restacks()
	parent := e.approvedTicket()
	child := e.stackedChild(parent)
	e.tick()
	if p, c := e.ticket(parent).Status, e.ticket(child).Status; p != store.TicketBuilding || c != store.TicketApproved {
		t.Fatalf("parent %s, child %s", p, c)
	}
	e.finishBuild(parent, githubtest.Head)
	e.tick() // the parent's checks are green: its merge waits for a person; the child builds on it
	if p, c := e.ticket(parent).Status, e.ticket(child).Status; p != store.TicketMerging || c != store.TicketBuilding {
		t.Fatalf("parent %s, child %s", p, c)
	}
	e.finishChild(child, githubtest.Head)
	e.tick()
	if tk := e.ticket(child); tk.Status != store.TicketReview || tk.MergeActionID != nil || len(*got) != 0 {
		t.Fatalf("a stacked child with green checks merges before its parent: %+v, restacks %v", tk, *got)
	}

	// The parent merges: the child moves onto main, its PR is retargeted,
	// and then it merges like any other.
	act := *e.ticket(parent).MergeActionID
	_ = e.s.DecideAction(ctx, act, true, "kanishk")
	claimed, _ := e.s.ClaimApprovedAction(ctx)
	_ = e.s.FinishAction(ctx, claimed.ID, json.RawMessage(`{"merged":true,"merge_sha":"`+mergeSHA+`"}`), nil)
	e.tick()
	if p := e.ticket(parent).Status; p != store.TicketDone {
		t.Fatalf("parent %s", p)
	}
	tk := e.ticket(child)
	if len(*got) != 1 || (*got)[0].Onto != "" || (*got)[0].From != githubtest.Head || (*got)[0].Head != childHead ||
		*tk.HeadSHA != "8888888888888888888888888888888888888888" || *tk.BaseSHA != "9999999999999999999999999999999999999999" {
		t.Fatalf("restacks %+v, child %+v", *got, tk)
	}
	e.gh.Mu.Lock()
	base := e.gh.Others[8]["base"].(map[string]any)["ref"]
	e.gh.Mu.Unlock()
	if base != "main" {
		t.Fatalf("PR 8's base = %v", base)
	}
	e.tick()
	if tk := e.ticket(child); tk.Status != store.TicketMerging || len(*got) != 1 {
		t.Fatalf("after the restack: %+v, restacks %d", tk, len(*got))
	}
	evs, _ := e.s.TicketEvents(ctx, child)
	found := false
	for _, ev := range evs {
		found = found || strings.Contains(string(ev.Detail), "restacked")
	}
	if !found {
		t.Fatal("the restack is not in the child's events")
	}
}

func TestAStackedTicketFollowsItsParentsNewHead(t *testing.T) {
	e := newEnv(t, "deny")
	got := e.restacks()
	parent := e.approvedTicket()
	child := e.stackedChild(parent)
	e.tick()
	e.finishBuild(parent, githubtest.Head)
	e.tick()
	e.finishChild(child, githubtest.Head)
	// The parent gets a fix: its head moves.
	const fixed = "6666666666666666666666666666666666666666"
	a := e.cfg.Agents["orbit"]
	if _, err := e.s.StartBuild(ctx, store.StartBuild{TicketID: parent, Agent: "orbit", SpecSHA: a.SHA, Purpose: store.BuildPurposeFix, Failure: json.RawMessage(`[]`)}); err != nil {
		t.Fatal(err)
	}
	e.finishBuild(parent, fixed)
	e.gh.Mu.Lock()
	e.gh.PR["head"].(map[string]any)["sha"] = fixed
	e.gh.Mu.Unlock()
	e.tick()
	if len(*got) != 1 || (*got)[0].Onto != "orbit/3f2a9c1e" || (*got)[0].From != githubtest.Head {
		t.Fatalf("restacks = %+v", *got)
	}
	if tk := e.ticket(child); tk.Status != store.TicketReview {
		t.Fatalf("child = %+v", tk)
	}
}

func TestAStackedTicketWaitsForItsParentsPR(t *testing.T) {
	e := newEnv(t, "approve")
	a := e.cfg.Agents["orbit"]
	p, _, _ := e.s.CreateTicket(ctx, store.NewTicket{Kind: "request", Title: "base", Body: "b", Agent: "orbit", SpecSHA: a.SHA, Repo: "kensac/orbit"})
	child := e.stackedChild(p.TicketID)
	e.tick()
	e.tick()
	if st := e.ticket(child).Status; st != store.TicketApproved {
		t.Fatalf("a child of an open ticket: %s", st)
	}
	// Abandoned below it: the child builds from the default branch.
	_ = e.s.TransitionTicket(ctx, p.TicketID, store.TicketAbandoned, "kanishk", store.TicketUpdate{})
	e.tick()
	if st := e.ticket(child).Status; st != store.TicketBuilding {
		t.Fatalf("a child of an abandoned ticket: %s", st)
	}
	for _, bad := range []store.NewTicket{
		{Kind: "request", Title: "x", Body: "b", Agent: "orbit", SpecSHA: a.SHA, Repo: "kensac/orbit", StackOn: p.TicketID},
		{Kind: "question", Title: "x", Body: "b", Agent: "orbit", SpecSHA: a.SHA, StackOn: child},
		{Kind: "request", Title: "x", Body: "b", Agent: "orbit", SpecSHA: a.SHA, Repo: "kensac/orbit", StackOn: "00000000-0000-0000-0000-000000000000"},
	} {
		if _, _, err := e.s.CreateTicket(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("stack on %s (%s) = %v", bad.StackOn, bad.Kind, err)
		}
	}
}
