package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// scriptRunner answers each run with what its agent's script returns for
// the request, and records every request by agent.
type scriptRunner struct {
	mu      sync.Mutex
	scripts map[string]func(req runner.Request) string
	got     map[string][]runner.Request
}

func (f *scriptRunner) Kind() string  { return "claude-code" }
func (f *scriptRunner) Model() string { return "fake" }
func (f *scriptRunner) Run(ctx context.Context, req runner.Request, emit func(runner.Event)) (*runner.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The charter names the agent: "I am <name>."
	name := ""
	for n := range f.scripts {
		if strings.Contains(req.Prompt, "I am "+n+".") {
			name = n
		}
	}
	if f.got == nil {
		f.got = map[string][]runner.Request{}
	}
	f.got[name] = append(f.got[name], req)
	emit(runner.Event{Kind: "assistant", Payload: []byte(`{"type":"assistant"}`)})
	cost := 0.01
	return &runner.Result{Output: withTaskOutcome(req, f.scripts[name](req)), CostUSD: &cost, SessionID: "s"}, nil
}

// withTaskOutcome gives a task run's output the outcome fields its schema
// requires, succeeded unless the script set them.
func withTaskOutcome(req runner.Request, out string) json.RawMessage {
	if !strings.Contains(string(req.OutputSchema), `"outcome"`) || strings.Contains(out, `"outcome"`) {
		return json.RawMessage(out)
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil {
		return json.RawMessage(out)
	}
	m["outcome"], m["outcome_reason"] = "succeeded", ""
	b, _ := json.Marshal(m)
	return b
}

// outcome is output with a task run's outcome.
func outcome(summary, report, result, reason string) string {
	var m map[string]any
	_ = json.Unmarshal([]byte(output(summary, report, nil)), &m)
	m["outcome"], m["outcome_reason"] = result, reason
	b, _ := json.Marshal(m)
	return string(b)
}

func (f *scriptRunner) last(name string) runner.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.got[name][len(f.got[name])-1]
}

func output(summary, report string, questions []string, acts ...string) string {
	q, _ := json.Marshal(questions)
	if questions == nil {
		q = []byte("[]")
	}
	return fmt.Sprintf(`{"summary": %q, "report": %q, "notify": {"send": true, "priority": "default", "title": ""},
		"memory": {"update": true, "content": %q}, "questions": %s, "actions": [%s]}`,
		summary, report, "remembered by "+summary, q, strings.Join(acts, ","))
}

func delegateAction(tasks string) string {
	return `{"type": "delegate", "params": {"tasks": ` + tasks + `}, "reason": "split the work"}`
}

type delegationHarness struct {
	e  *engine.Engine
	s  *store.Store
	fr *scriptRunner
	h  *hooks
}

const plannerSpec = `
name: planner
charter: I am planner.
triggers: []
wake_on_message: true
delegates: [researcher, writer]
actions: {delegate: approve, continue: auto}
`

func newDelegation(t *testing.T, fr *scriptRunner) *delegationHarness {
	t.Helper()
	return newDelegationWith(t, fr, plannerSpec)
}

func newDelegationWith(t *testing.T, fr *scriptRunner, planner string) *delegationHarness {
	t.Helper()
	s := testdb.New(t)
	h := &hooks{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		h.mu.Lock()
		h.messages = append(h.messages, m)
		h.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	write := func(name, body string) {
		p := filepath.Join(dir, name)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("orbit.yaml", fmt.Sprintf(`
timezone: UTC
public_url: http://orbit.test
defaults: {runner: fake, channels: [hook]}
runners: {fake: {kind: claude-code}}
channels:
  hook: {kind: webhook, url: %s/notify}
`, srv.URL))
	write("agents/planner.yaml", planner)
	for _, n := range []string{"researcher", "writer"} {
		write("agents/"+n+".yaml", fmt.Sprintf("name: %[1]s\ncharter: I am %[1]s.\ntriggers: []\n", n))
	}
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &engine.Engine{
		Store: s, DataDir: t.TempDir(), Env: func(string) (string, bool) { return "", false }, Client: srv.Client(),
		Secret:    []byte("secret"),
		NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) { return fr, nil },
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	specs := map[string]store.AgentSpec{}
	for n, a := range cfg.Agents {
		b, _ := json.Marshal(a)
		specs[n] = store.AgentSpec{SHA: a.SHA, JSON: b, Enabled: true}
	}
	if err := s.SyncAgents(ctx, specs); err != nil {
		t.Fatal(err)
	}
	return &delegationHarness{e: e, s: s, fr: fr, h: h}
}

// step executes approved actions, reconciles, then claims and executes
// every run due, until nothing is due; it returns the runs it executed by
// agent, in order.
func (d *delegationHarness) step(t *testing.T) map[string][]*store.ClaimedRun {
	t.Helper()
	ran := map[string][]*store.ClaimedRun{}
	for range 20 {
		if err := d.e.ExecuteActions(ctx); err != nil {
			t.Fatal(err)
		}
		if err := d.e.ReconcileTasks(ctx); err != nil {
			t.Fatal(err)
		}
		c, err := d.s.ClaimRun(ctx, "test")
		if err != nil {
			t.Fatal(err)
		}
		if c == nil {
			return ran
		}
		if err := d.e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		ran[c.Agent] = append(ran[c.Agent], c)
	}
	t.Fatal("runs did not settle")
	return nil
}

func (d *delegationHarness) message(t *testing.T, body string) string {
	t.Helper()
	a := d.e.Current().Config.Agents["planner"]
	r, _, err := d.s.PostMessageOnce(ctx, store.PostMessage{Agent: "planner", SpecSHA: a.SHA, Body: body, Author: "kanishk", Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	return r.RunID
}

func (d *delegationHarness) tasks(t *testing.T, runID string) []store.Task {
	t.Helper()
	ts, err := d.s.ListTasksForRuns(ctx, []string{runID})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func TestAParentDelegatesTwoTasksAndIsWokenWithBothReports(t *testing.T) {
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{
		"planner": func(req runner.Request) string {
			if strings.Contains(req.Prompt, "Trigger: delegation") {
				return output("Integrated", "both reports in", nil)
			}
			return output("Delegated", "on it", nil, delegateAction(`[
				{"key": "facts", "agent": "researcher", "brief": "Find the facts."},
				{"key": "outline", "agent": "writer", "brief": "Draft an outline."}]`))
		},
		"researcher": func(runner.Request) string { return output("Facts found", "FACTS-REPORT", nil) },
		"writer":     func(runner.Request) string { return output("Outline drafted", "OUTLINE-REPORT", nil) },
	}}
	d := newDelegation(t, fr)
	first := d.message(t, "write me a piece")

	// The person's own conversation: delegate's approve rule applies
	// directly.
	c, _ := d.s.ClaimRun(ctx, "test")
	if err := d.e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	acts, _ := d.s.ListActions(ctx, "planner", "", 0)
	if len(acts) != 1 || acts[0].Status != "approved" || acts[0].Rule != "auto" {
		t.Fatalf("delegate in an attended run = %+v", acts)
	}
	if err := d.e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	ts := d.tasks(t, first)
	if len(ts) != 2 || ts[0].Status != store.TaskWorking || ts[1].Status != store.TaskWorking {
		t.Fatalf("tasks after delegate = %+v", ts)
	}
	// Two agents: both run at once.
	a, _ := d.s.ClaimRun(ctx, "w1")
	b, _ := d.s.ClaimRun(ctx, "w2")
	if a == nil || b == nil || a.Agent == b.Agent || a.Trigger != store.TriggerTask {
		t.Fatalf("parallel claims = %+v %+v", a, b)
	}
	for _, c := range []*store.ClaimedRun{a, b} {
		if err := d.e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	req := fr.last("researcher")
	if !strings.Contains(req.System, `delegated to you`) || !strings.Contains(req.Prompt, "from planner: Find the facts.") {
		t.Fatalf("the child is not told whom it reports to:\n%s\n%s", req.System, req.Prompt)
	}
	ran := d.step(t)
	if len(ran["planner"]) != 1 || ran["planner"][0].Trigger != store.TriggerDelegation {
		t.Fatalf("parent wakes = %+v", ran)
	}
	p := fr.last("planner").Prompt
	for _, want := range []string{"FACTS-REPORT", "OUTLINE-REPORT", "## facts: done", "## outline: done", "Trigger: delegation"} {
		if !strings.Contains(p, want) {
			t.Errorf("parent prompt lacks %q:\n%s", want, p)
		}
	}
	ts = d.tasks(t, first)
	if ts[0].Report == nil || ts[0].Report.Report != "FACTS-REPORT" || ts[0].Status != store.TaskDone {
		t.Fatalf("report = %+v", ts[0])
	}
	// Woken once: a settled batch is not reported again.
	if ran := d.step(t); len(ran) != 0 {
		t.Fatalf("woken again: %+v", ran)
	}
	// The task's report and question notifications went nowhere; the
	// parent's runs answered the person's conversation or were unattended.
	d.h.mu.Lock()
	for _, m := range d.h.messages {
		if title, _ := m["title"].(string); strings.Contains(title, "researcher") || strings.Contains(title, "writer") {
			t.Errorf("a task's run notified the person: %v", m)
		}
	}
	d.h.mu.Unlock()
	// The thread shows the tasks on the run that delegated them.
	tr, err := d.s.ThreadRunByID(ctx, first)
	if err != nil || len(tr.Tasks) != 2 || tr.Tasks[0].Key != "facts" || tr.Tasks[0].Status != "done" ||
		tr.Tasks[0].Summary == nil || *tr.Tasks[0].Summary != "Facts found" {
		t.Fatalf("thread tasks = %+v %v", tr, err)
	}
}

func TestAnUnattendedRunsDelegationWaitsForApproval(t *testing.T) {
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{
		"planner": func(runner.Request) string {
			return output("Delegated", "on it", nil, delegateAction(`[{"key": "facts", "agent": "researcher", "brief": "x"}]`))
		},
		"researcher": func(runner.Request) string { return output("done", "r", nil) },
	}}
	d := newDelegation(t, fr)
	a := d.e.Current().Config.Agents["planner"]
	if _, err := d.s.EnqueueRun(ctx, store.NewRun{Agent: "planner", SpecSHA: a.SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	d.step(t)
	acts, _ := d.s.ListActions(ctx, "planner", "", 0)
	if len(acts) != 1 || acts[0].Status != "pending" {
		t.Fatalf("delegate in an unattended run = %+v", acts)
	}
	if len(fr.got["researcher"]) != 0 {
		t.Fatal("a task started before its delegation was approved")
	}
}

func TestADependentTaskStartsWithItsDependencysReport(t *testing.T) {
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{
		"planner": func(req runner.Request) string {
			if strings.Contains(req.Prompt, "Trigger: delegation") {
				return output("Integrated", "ok", nil)
			}
			return output("Delegated", "on it", nil, delegateAction(`[
				{"key": "draft", "agent": "writer", "brief": "Write it up.", "after": ["facts"]},
				{"key": "facts", "agent": "researcher", "brief": "Find the facts."}]`))
		},
		"researcher": func(runner.Request) string { return output("Facts found", "FACTS-REPORT", nil) },
		"writer":     func(runner.Request) string { return output("Drafted", "DRAFT", nil) },
	}}
	d := newDelegation(t, fr)
	first := d.message(t, "go")
	c, _ := d.s.ClaimRun(ctx, "test")
	if err := d.e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := d.e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	ts := d.tasks(t, first)
	if ts[0].Key != "draft" || ts[0].Status != store.TaskWaiting || ts[1].Status != store.TaskWorking {
		t.Fatalf("before facts is done: %+v", ts)
	}
	ran := d.step(t)
	if len(ran["researcher"]) != 1 || len(ran["writer"]) != 1 || len(ran["planner"]) != 1 {
		t.Fatalf("ran = %+v", ran)
	}
	if !strings.Contains(fr.last("writer").Prompt, "FACTS-REPORT") {
		t.Fatalf("the dependent was not given its dependency's report:\n%s", fr.last("writer").Prompt)
	}
}

func TestAChildsQuestionGoesToTheParentWhichReplies(t *testing.T) {
	var questionID string
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{}}
	fr.scripts["planner"] = func(req runner.Request) string {
		switch {
		case strings.Contains(req.Prompt, "## facts: needs_input"):
			i := strings.Index(req.Prompt, "question (reply_to ")
			questionID = req.Prompt[i+len("question (reply_to ") : i+len("question (reply_to ")+36]
			j := strings.Index(req.Prompt, "task_id: ")
			taskID := req.Prompt[j+9 : j+9+36]
			return output("Answered", "told it", nil, fmt.Sprintf(`{"type": "task:message", "params": {"task_id": %q, "body": "Use 2025.", "reply_to": %q}, "reason": "answer"}`, taskID, questionID))
		case strings.Contains(req.Prompt, "Trigger: delegation"):
			return output("Integrated", "ok", nil)
		}
		return output("Delegated", "on it", nil, delegateAction(`[{"key": "facts", "agent": "researcher", "brief": "Find the facts."}]`))
	}
	fr.scripts["researcher"] = func(req runner.Request) string {
		if strings.Contains(req.Prompt, "Use 2025.") {
			return output("Facts for 2025", "FACTS-2025", nil)
		}
		return output("Need a year", "which year?", []string{"Which year?"})
	}
	d := newDelegation(t, fr)
	first := d.message(t, "go")
	d.step(t)
	// The parent was woken by the question while unattended, so its reply
	// follows its rule: approve.
	pending, _ := d.s.ListActions(ctx, "planner", "pending", 0)
	if len(pending) != 1 || pending[0].Type != "task:message" {
		t.Fatalf("pending = %+v", pending)
	}
	if err := d.s.DecideAction(ctx, pending[0].ID, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	d.step(t)
	ts := d.tasks(t, first)
	if ts[0].Status != store.TaskDone || ts[0].Report.Report != "FACTS-2025" || questionID == "" {
		t.Fatalf("task = %+v (question %q)", ts[0], questionID)
	}
	// The child's question reached the parent, not the person: only the
	// parent's own unattended runs notified.
	d.h.mu.Lock()
	for _, m := range d.h.messages {
		if title, _ := m["title"].(string); !strings.HasPrefix(title, "planner") {
			t.Errorf("the task's run notified the person: %v", m)
		}
	}
	d.h.mu.Unlock()
	agents, _ := d.s.ListAgents(ctx)
	for _, a := range agents {
		if a.NeedsInput {
			t.Errorf("%s needs the person's input for a question to its parent", a.Name)
		}
	}
	if !strings.Contains(fr.last("researcher").Prompt, "from planner: Use 2025.") {
		t.Fatalf("child prompt:\n%s", fr.last("researcher").Prompt)
	}
	if !strings.Contains(fr.last("planner").Prompt, "FACTS-2025") {
		t.Fatalf("the parent was not woken with the final report:\n%s", fr.last("planner").Prompt)
	}
}

func TestADecisionGateHoldsItsTaskUntilAPersonResolvesIt(t *testing.T) {
	for _, approve := range []bool{true, false} {
		fr := &scriptRunner{scripts: map[string]func(runner.Request) string{
			"planner": func(req runner.Request) string {
				if strings.Contains(req.Prompt, "Trigger: delegation") {
					return output("Integrated", "ok", nil)
				}
				return output("Delegated", "on it", nil, delegateAction(`[
					{"key": "spend", "agent": "researcher", "brief": "Buy the data.", "decision": "Spend $20 on the dataset?"},
					{"key": "write", "agent": "writer", "brief": "Write.", "after": ["spend"]}]`))
			},
			"researcher": func(runner.Request) string { return output("Bought", "DATA", nil) },
			"writer":     func(runner.Request) string { return output("Written", "W", nil) },
		}}
		d := newDelegation(t, fr)
		first := d.message(t, "go")
		d.step(t)
		ts := d.tasks(t, first)
		if ts[0].Status != store.TaskBlocked || ts[0].Decision == nil || ts[0].Decision.Status != "pending" {
			t.Fatalf("gated task = %+v", ts[0])
		}
		// Re-checked every reconcile: still blocked.
		if ran := d.step(t); len(ran) != 0 {
			t.Fatalf("ran while blocked: %+v", ran)
		}
		if err := d.s.DecideAction(ctx, ts[0].Decision.ActionID, approve, "kanishk"); err != nil {
			t.Fatal(err)
		}
		ran := d.step(t)
		ts = d.tasks(t, first)
		if approve {
			if ts[0].Status != store.TaskDone || ts[1].Status != store.TaskDone || len(ran["planner"]) != 1 {
				t.Fatalf("approved: %+v %+v", ts, ran)
			}
			if !strings.Contains(fr.last("researcher").Prompt, "approved this task") {
				t.Errorf("the task was not told of the decision:\n%s", fr.last("researcher").Prompt)
			}
		} else {
			if ts[0].Status != store.TaskCancelled || ts[1].Status != store.TaskCancelled || len(ran["planner"]) != 1 {
				t.Fatalf("rejected: %+v %+v", ts, ran)
			}
			if !strings.Contains(*ts[1].Error, `dependency "spend" cancelled`) {
				t.Errorf("dependent error = %s", *ts[1].Error)
			}
		}
	}
}

func TestAFreshWorkerNeitherReadsNorWritesItsAgentsMemory(t *testing.T) {
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{
		"planner": func(req runner.Request) string {
			if strings.Contains(req.Prompt, "Trigger: delegation") {
				return output("Integrated", "ok", nil)
			}
			return output("Delegated", "on it", nil, delegateAction(`[{"key": "w", "agent": "researcher", "brief": "x", "fresh": true}]`))
		},
		"researcher": func(runner.Request) string { return output("done", "r", nil) },
	}}
	d := newDelegation(t, fr)
	if _, err := d.s.Pool.Exec(ctx, `INSERT INTO memory_versions (agent, content) VALUES ('researcher', 'SECRET-MEMORY')`); err != nil {
		t.Fatal(err)
	}
	d.message(t, "go")
	d.step(t)
	if strings.Contains(fr.last("researcher").Prompt, "SECRET-MEMORY") {
		t.Fatal("a fresh worker read its agent's memory")
	}
	if m, _ := d.s.Memory(ctx, "researcher"); m != "SECRET-MEMORY" {
		t.Fatalf("a fresh worker wrote its agent's memory: %q", m)
	}
}

func TestATaskCannotDelegateDeeperThanTheBound(t *testing.T) {
	// planner delegates to itself; each level delegates again.
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{}}
	fr.scripts["planner"] = func(req runner.Request) string {
		if strings.Contains(req.Prompt, "Trigger: delegation") {
			return output("Integrated", "ok", nil)
		}
		return output("Delegated", "deeper", nil, delegateAction(`[{"key": "deeper", "agent": "planner", "brief": "go deeper"}]`))
	}
	d := newDelegationWith(t, fr, strings.NewReplacer("[researcher, writer]", `["*"]`, "delegate: approve", "delegate: auto").Replace(plannerSpec))
	d.message(t, "go")
	d.step(t)
	var depth int
	if err := d.s.Pool.QueryRow(ctx, `SELECT max(depth) FROM tasks`).Scan(&depth); err != nil {
		t.Fatal(err)
	}
	if depth != store.MaxTaskDepth {
		t.Fatalf("deepest task = %d, want %d", depth, store.MaxTaskDepth)
	}
	var denied int
	if err := d.s.Pool.QueryRow(ctx, `SELECT count(*) FROM actions WHERE type = 'delegate' AND status = 'denied' AND error LIKE 'delegation is nested%'`).Scan(&denied); err != nil || denied != 1 {
		t.Fatalf("denied at the bound = %d %v", denied, err)
	}
}

func TestATaskThatSaysItCouldNotDoTheWorkFailsAndOneBlockedAsksItsParent(t *testing.T) {
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{
		"planner": func(req runner.Request) string {
			if strings.Contains(req.Prompt, "Trigger: delegation") {
				return output("Integrated", "ok", nil)
			}
			return output("Delegated", "on it", nil, delegateAction(`[
				{"key": "facts", "agent": "researcher", "brief": "Find the facts."},
				{"key": "outline", "agent": "writer", "brief": "Draft an outline."}]`))
		},
		"researcher": func(runner.Request) string {
			return outcome("No facts", "TRIED-THREE-SOURCES", "failed", "every source was down")
		},
		"writer": func(runner.Request) string { return outcome("Waiting", "W", "blocked", "needs the style guide") },
	}}
	d := newDelegation(t, fr)
	first := d.message(t, "go")
	ran := d.step(t)
	if !strings.Contains(string(fr.last("researcher").OutputSchema), `"outcome_reason"`) {
		t.Fatal("a task run's schema lacks the outcome")
	}
	if strings.Contains(string(fr.got["planner"][0].OutputSchema), `"outcome"`) {
		t.Fatal("a run outside a task was offered the outcome")
	}
	ts := d.tasks(t, first)
	facts, outline := ts[0], ts[1]
	if facts.Status != store.TaskFailed || facts.Error == nil || *facts.Error != "the agent could not do the work: every source was down" ||
		facts.Report == nil || facts.Report.Report != "TRIED-THREE-SOURCES" || facts.Outcome == nil || *facts.Outcome != "failed" {
		t.Fatalf("failed outcome = %+v", facts)
	}
	if outline.Status != store.TaskNeedsInput || outline.OutcomeReason == nil || *outline.OutcomeReason != "needs the style guide" {
		t.Fatalf("blocked outcome = %+v", outline)
	}
	// Blocked is due to the parent at once, and its prompt says why.
	if len(ran["planner"]) != 2 || ran["planner"][1].Trigger != store.TriggerDelegation {
		t.Fatalf("parent wakes = %+v", ran["planner"])
	}
	p := fr.last("planner").Prompt
	for _, want := range []string{"## facts: failed", "outcome: failed: every source was down", "TRIED-THREE-SOURCES", "## outline: needs_input", "outcome: blocked: needs the style guide"} {
		if !strings.Contains(p, want) {
			t.Errorf("parent prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestAFailedTaskWakesItsParentAtOnceWhileItsSiblingRuns(t *testing.T) {
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{
		"planner": func(req runner.Request) string {
			if strings.Contains(req.Prompt, "Trigger: delegation") {
				return output("Integrated", "ok", nil)
			}
			return output("Delegated", "on it", nil, delegateAction(`[
				{"key": "facts", "agent": "researcher", "brief": "Find the facts."},
				{"key": "outline", "agent": "writer", "brief": "Draft an outline."}]`))
		},
		"researcher": func(runner.Request) string { return outcome("No facts", "R", "failed", "the source is gone") },
		"writer":     func(runner.Request) string { return output("Outline drafted", "OUTLINE-REPORT", nil) },
	}}
	d := newDelegation(t, fr)
	first := d.message(t, "go")
	run := func(want string) {
		t.Helper()
		c, err := d.s.ClaimRun(ctx, "test")
		if err != nil || c == nil || c.Agent != want {
			t.Fatalf("claimed %+v (%v), want a run of %s", c, err, want)
		}
		if err := d.e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		if err := d.e.ExecuteActions(ctx); err != nil {
			t.Fatal(err)
		}
		if err := d.e.ReconcileTasks(ctx); err != nil {
			t.Fatal(err)
		}
	}
	waiting := func() (n int) {
		t.Helper()
		if err := d.s.Pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE agent = 'planner' AND trigger = 'delegation' AND status = 'queued'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	run("planner")
	run("researcher")
	ts := d.tasks(t, first)
	if ts[0].Status != store.TaskFailed || ts[1].Status != store.TaskWorking {
		t.Fatalf("after the failure: %+v", ts)
	}
	if waiting() != 1 {
		t.Fatal("the parent was not woken at the failure")
	}
	// The sibling settles the batch while that wake still waits: one
	// waiting wake, which reads both.
	run("writer")
	if n := waiting(); n != 1 {
		t.Fatalf("waiting wakes = %d, want 1", n)
	}
	run("planner")
	p := fr.last("planner").Prompt
	for _, want := range []string{"## facts: failed", "the source is gone", "## outline: done", "OUTLINE-REPORT"} {
		if !strings.Contains(p, want) {
			t.Errorf("parent prompt lacks %q:\n%s", want, p)
		}
	}
	if ran := d.step(t); len(ran) != 0 {
		t.Fatalf("woken again: %+v", ran)
	}
}

const retryingPlanner = `
name: planner
charter: I am planner.
triggers: []
wake_on_message: true
delegates: [researcher, writer]
actions: {delegate: approve, task:retry: auto}
`

// taskIDIn reads the task_id of the task with key from a parent's prompt.
func taskIDIn(prompt, key string) string {
	i := strings.Index(prompt, "## "+key+": ")
	if i < 0 {
		return ""
	}
	j := strings.Index(prompt[i:], "task_id: ")
	return prompt[i+j+9 : i+j+9+36]
}

func retryAction(taskID, extra string) string {
	return fmt.Sprintf(`{"type": "task:retry", "params": {"task_id": %q, "reason": "the archive has it"%s}, "reason": "try again"}`, taskID, extra)
}

func TestARetryWithANewBriefReopensTheSameTaskAndItsDependentGoesOn(t *testing.T) {
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{
		"planner": func(req runner.Request) string {
			switch {
			case strings.Contains(req.Prompt, "## facts: failed"):
				return output("Retrying", "again", nil, retryAction(taskIDIn(req.Prompt, "facts"), `, "brief": "Find the facts in the archive."`))
			case strings.Contains(req.Prompt, "Trigger: delegation"):
				return output("Integrated", "ok", nil)
			}
			return output("Delegated", "on it", nil, delegateAction(`[
				{"key": "facts", "agent": "researcher", "brief": "Find the facts."},
				{"key": "draft", "agent": "writer", "brief": "Write it up.", "after": ["facts"]}]`))
		},
		"researcher": func(req runner.Request) string {
			if strings.Contains(req.Prompt, "in the archive") {
				return output("Facts found", "FACTS-FROM-ARCHIVE", nil)
			}
			return outcome("No facts", "R", "failed", "the source is gone")
		},
		"writer": func(req runner.Request) string { return output("Drafted", "DRAFT", nil) },
	}}
	d := newDelegationWith(t, fr, retryingPlanner)
	first := d.message(t, "go")
	ran := d.step(t)
	if len(ran["researcher"]) != 2 || len(ran["writer"]) != 1 {
		t.Fatalf("ran = %+v", ran)
	}
	ts := d.tasks(t, first)
	facts, draft := ts[0], ts[1]
	if facts.Status != store.TaskDone || facts.Attempt != 2 || facts.Failures != 1 || facts.Brief != "Find the facts in the archive." ||
		len(facts.Attempts) != 1 || facts.Attempts[0].Error == nil || !strings.Contains(*facts.Attempts[0].Error, "the source is gone") ||
		facts.Attempts[0].Brief != "Find the facts." {
		t.Fatalf("retried task = %+v", facts)
	}
	// The dependent waited through the failure instead of being cancelled,
	// and started with the retried report.
	if draft.Status != store.TaskDone || !strings.Contains(fr.last("writer").Prompt, "FACTS-FROM-ARCHIVE") {
		t.Fatalf("dependent = %+v\n%s", draft, fr.last("writer").Prompt)
	}
	p := fr.last("researcher").Prompt
	for _, want := range []string{"Try this task again (attempt 2)", "the source is gone", "Why again: the archive has it", "The brief is now:"} {
		if !strings.Contains(p, want) {
			t.Errorf("the retried run's prompt lacks %q:\n%s", want, p)
		}
	}
}

func TestARetryToAnotherAgentMovesTheTaskToANewConversation(t *testing.T) {
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{
		"planner": func(req runner.Request) string {
			switch {
			case strings.Contains(req.Prompt, "## facts: failed"):
				return output("Rerouting", "again", nil, retryAction(taskIDIn(req.Prompt, "facts"), `, "agent": "writer"`))
			case strings.Contains(req.Prompt, "Trigger: delegation"):
				return output("Integrated", "ok", nil)
			}
			return output("Delegated", "on it", nil, delegateAction(`[{"key": "facts", "agent": "researcher", "brief": "Find the facts."}]`))
		},
		"researcher": func(runner.Request) string { return outcome("No facts", "R", "failed", "out of my depth") },
		"writer":     func(runner.Request) string { return output("Facts found", "WRITER-FACTS", nil) },
	}}
	d := newDelegationWith(t, fr, retryingPlanner)
	first := d.message(t, "go")
	d.step(t)
	ts := d.tasks(t, first)
	f := ts[0]
	if f.Status != store.TaskDone || f.Agent != "writer" || f.Report.Report != "WRITER-FACTS" || len(f.Attempts) != 1 ||
		f.Attempts[0].Agent != "researcher" || f.Attempts[0].ConversationID == f.ConversationID {
		t.Fatalf("rerouted task = %+v", f)
	}
	p := fr.last("writer").Prompt
	if !strings.Contains(p, "from planner: Find the facts.") || !strings.Contains(p, "Another agent (researcher) tried this task") {
		t.Fatalf("the new agent's prompt:\n%s", p)
	}
	// The earlier attempt's conversation keeps its rows.
	var n int
	if err := d.s.Pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE conversation_id = $1`, f.Attempts[0].ConversationID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("runs in the first attempt's conversation = %d %v", n, err)
	}
}

func TestTheCircuitBreakerHoldsARetryForAPersonAfterThreeFailures(t *testing.T) {
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{}}
	fr.scripts["planner"] = func(req runner.Request) string {
		if strings.Contains(req.Prompt, "## facts: failed") {
			return output("Retrying", "again", nil, retryAction(taskIDIn(req.Prompt, "facts"), ""))
		}
		return output("Delegated", "on it", nil, delegateAction(`[{"key": "facts", "agent": "researcher", "brief": "Find the facts."}]`))
	}
	fr.scripts["researcher"] = func(runner.Request) string { return outcome("No facts", "R", "failed", "still gone") }
	d := newDelegationWith(t, fr, retryingPlanner)
	first := d.message(t, "go")
	d.step(t)
	ts := d.tasks(t, first)
	if ts[0].Status != store.TaskFailed || ts[0].Failures != store.MaxTaskFailures || ts[0].Attempt != store.MaxTaskFailures {
		t.Fatalf("after the breaker: %+v", ts[0])
	}
	if !strings.Contains(fr.last("planner").Prompt, "only a person's approval reopens it") {
		t.Errorf("the parent was not told of the breaker:\n%s", fr.last("planner").Prompt)
	}
	// Its rule is auto, yet the retry past the breaker waits for a person.
	pending, _ := d.s.ListActions(ctx, "planner", "pending", 0)
	if len(pending) != 1 || pending[0].Type != "task:retry" || pending[0].Rule != "approve" {
		t.Fatalf("pending = %+v", pending)
	}
	// Executed without a person, the store refuses it too.
	if _, err := d.s.RetryTask(ctx, store.TaskRetry{FromRun: pending[0].RunID, ActionID: pending[0].ID, TaskID: ts[0].ID, Reason: "x"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("retry past the breaker without a person = %v", err)
	}
	// A person's approval reopens it.
	fr.scripts["researcher"] = func(runner.Request) string { return output("Facts found", "FINALLY", nil) }
	if err := d.s.DecideAction(ctx, pending[0].ID, true, "kanishk"); err != nil {
		t.Fatal(err)
	}
	d.step(t)
	ts = d.tasks(t, first)
	if ts[0].Status != store.TaskDone || ts[0].Attempt != store.MaxTaskFailures+1 || len(ts[0].Attempts) != store.MaxTaskFailures {
		t.Fatalf("after a person's approval: %+v", ts[0])
	}
}

func TestARunLostWithItsWorkerDoesNotCountTowardTheBreaker(t *testing.T) {
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{
		"planner": func(req runner.Request) string {
			if strings.Contains(req.Prompt, "Trigger: delegation") {
				return output("Integrated", "ok", nil)
			}
			return output("Delegated", "on it", nil, delegateAction(`[{"key": "facts", "agent": "researcher", "brief": "Find the facts."}]`))
		},
		"researcher": func(runner.Request) string { return output("Facts found", "F", nil) },
	}}
	d := newDelegation(t, fr)
	first := d.message(t, "go")
	c, _ := d.s.ClaimRun(ctx, "test")
	if err := d.e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := d.e.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	// Its worker dies on every attempt; reclaims keep the task working.
	for i := 1; i <= store.MaxAttempts; i++ {
		c, err := d.s.ClaimRun(ctx, "doomed")
		if err != nil || c == nil || c.Agent != "researcher" {
			t.Fatalf("claim %d = %+v %v", i, c, err)
		}
		if _, err := d.s.Pool.Exec(ctx, `UPDATE runs SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := d.s.ReapStale(ctx, time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := d.e.ReconcileTasks(ctx); err != nil {
			t.Fatal(err)
		}
		ts := d.tasks(t, first)
		if i < store.MaxAttempts && ts[0].Status != store.TaskWorking {
			t.Fatalf("after reclaim %d: %+v", i, ts[0])
		}
	}
	ts := d.tasks(t, first)
	if ts[0].Status != store.TaskFailed || ts[0].Failures != 0 {
		t.Fatalf("a task whose worker was lost = %+v", ts[0])
	}
}
