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
	"time"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// A parent's task:message to a task its chat agent is working on goes into that agent's running turn (steered); with nothing running, or to a done task, it is queued for the task's next run. A steer the turn did not take wakes a task run, not a person's.
func TestATaskMessageSteersTheWorkersRunningTurn(t *testing.T) {
	s := testdb.New(t)
	dir := t.TempDir()
	cli := filepath.Join(dir, "claude")
	if err := os.WriteFile(cli, []byte(steerCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(`
defaults: {runner: fake, channels: [log]}
runners: {fake: {kind: claude-code}, cc: {kind: claude-code, binary: `+cli+`}}
channels: {log: {kind: log}}
`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "planner.yaml"), []byte(`
name: planner
charter: I am planner.
wake_on_message: true
delegates: [helper]
actions: {delegate: approve}
`), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "agents", "helper.yaml"), []byte(`
name: helper
charter: Help.
runner: cc
chat: true
session: {resume: true, idle: 1h, live: 5m}
`), 0o644)
	// The planner delegates one task, then sends it the text after the newest SEND in its prompt.
	fr := &scriptRunner{scripts: map[string]func(runner.Request) string{"planner": func(req runner.Request) string {
		if strings.Contains(req.Prompt, "Trigger: delegation") {
			return output("Integrated", "ok", nil)
		}
		if i := strings.LastIndex(req.Prompt, "SEND "); i >= 0 {
			body, _, _ := strings.Cut(req.Prompt[i+5:], "\n")
			return output("Sent", "sent", nil, fmt.Sprintf(`{"type": "task:message", "params": {"task_id": %q, "body": %q}, "reason": "correct it"}`,
				taskIDIn(req.Prompt, "fix"), body))
		}
		return output("Delegated", "on it", nil, delegateAction(`[{"key": "fix", "agent": "helper", "brief": "fix it WAIT"}]`))
	}}}
	env := func(k string) (string, bool) {
		v, ok := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-1"}[k]
		return v, ok
	}
	e := &engine.Engine{Store: s, DataDir: t.TempDir(), ConfigDir: dir, Env: env, Secret: []byte("secret"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Live: runner.NewLive(2),
		NewRunner: func(name string, rs spec.RunnerSpec, env func(string) (string, bool)) (runner.Runner, error) {
			if name == "fake" {
				return fr, nil
			}
			return runner.New(name, rs, env)
		}}
	defer e.Live.Close()
	if err := e.Reload(ctx); err != nil {
		t.Fatal(err)
	}

	claim := func(agent string) *store.ClaimedRun {
		t.Helper()
		c, err := s.ClaimRun(ctx, "test")
		if err != nil || c == nil || c.Agent != agent {
			t.Fatalf("claim = %+v %v, want a run of %s", c, err, agent)
		}
		return c
	}
	// plan has its person write to the planner and runs its reply; it returns the run.
	plan := func(body string) string {
		t.Helper()
		a := e.Current().Config.Agents["planner"]
		if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "planner", SpecSHA: a.SHA, Body: body, Author: "kanishk", Wake: true}); err != nil {
			t.Fatal(err)
		}
		c := claim("planner")
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	act := func() {
		t.Helper()
		if err := e.ExecuteActions(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// planner is plan, then the action loop executing what it proposed.
	planner := func(body string) string {
		t.Helper()
		id := plan(body)
		act()
		return id
	}
	// sent is the result of the task:message the planner's run executed.
	sent := func(runID string) store.TaskMessaged {
		t.Helper()
		var raw []byte
		var status string
		if err := s.Pool.QueryRow(ctx, `SELECT status, result FROM actions WHERE run_id = $1 AND type = 'task:message'`, runID).Scan(&status, &raw); err != nil {
			t.Fatal(err)
		}
		var r store.TaskMessaged
		if err := json.Unmarshal(raw, &r); err != nil || status != "executed" {
			t.Fatalf("task:message %s: %s %v", status, raw, err)
		}
		return r
	}
	start := func() (*store.ClaimedRun, chan error) {
		t.Helper()
		c := claim("helper")
		done := make(chan error, 1)
		go func() { done <- e.Execute(ctx, c) }()
		waitEvent(t, s, c.ID, "assistant", "tool_use")
		return c, done
	}
	finish := func(c *store.ClaimedRun, done chan error) *store.ThreadRun {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the task's run did not finish")
		}
		run, err := s.ThreadRunByID(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		return run
	}
	task := func() store.Task {
		t.Helper()
		var id string
		if err := s.Pool.QueryRow(ctx, `SELECT id::text FROM tasks WHERE key = 'fix'`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return *got
	}
	queuedHelper := func() (ids []string, triggers []string) {
		t.Helper()
		rows, err := s.Pool.Query(ctx, `SELECT id::text, trigger FROM runs WHERE agent = 'helper' AND status = 'queued' ORDER BY created_at`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, tr string
			_ = rows.Scan(&id, &tr)
			ids, triggers = append(ids, id), append(triggers, tr)
		}
		return ids, triggers
	}

	planner("go")
	if err := e.ReconcileTasks(ctx); err != nil {
		t.Fatal(err)
	}

	// Steered while working: the correction reaches the turn in progress, named as the parent's, and wakes nothing.
	c, done := start()
	fix := sent(planner("SEND fold: use 2025"))
	if fix.Delivered != store.DeliveredSteered || fix.RunID != c.ID {
		t.Fatalf("task:message to a working task = %+v, want steered into %s", fix, c.ID)
	}
	if ids, _ := queuedHelper(); len(ids) != 0 {
		t.Fatalf("a steered task:message queued %v", ids)
	}
	run := finish(c, done)
	if run.Status != "succeeded" || deref(run.Report) != "answered and from planner: fold: use 2025" {
		t.Fatalf("run = %s %q %q", run.Status, deref(run.Report), deref(run.Error))
	}
	var into *string
	if err := s.Pool.QueryRow(ctx, `SELECT steered_into::text FROM messages WHERE id = $1`, fix.MessageID).Scan(&into); err != nil || deref(into) != c.ID {
		t.Fatalf("steered_into = %v %v", into, err)
	}
	var author string
	if err := s.Pool.QueryRow(ctx, `SELECT body->>'author' FROM journal_items WHERE run_id = $1 AND kind = 'steer'`, c.ID).Scan(&author); err != nil || author != "agent:planner" {
		t.Fatalf("steer item author = %q %v", author, err)
	}
	if err := e.ReconcileTasks(ctx); err != nil {
		t.Fatal(err)
	}
	if got := task(); got.Status != store.TaskDone {
		t.Fatalf("task = %s, want done", got.Status)
	}
	if err := e.Execute(ctx, claim("planner")); err != nil { // woken by the task's report
		t.Fatal(err)
	}

	// Two replies to a done task, their actions executed together: one reopens the task with a run to read it, the other finds that run queued and nothing running. Both are queued for that run.
	again, more := plan("SEND again WAIT"), plan("SEND more WAIT")
	act()
	ids, triggers := queuedHelper()
	for _, r := range []store.TaskMessaged{sent(again), sent(more)} {
		if r.Delivered != store.DeliveredQueued || len(ids) != 1 || r.RunID != ids[0] || triggers[0] != "task" {
			t.Fatalf("task:message to a done task = %+v, queued %v %v", r, ids, triggers)
		}
	}
	if got := task(); got.Status != store.TaskWorking {
		t.Fatalf("task = %s, want reopened", got.Status)
	}

	// A steer the turn did not take wakes a task run, so the parent's message never runs as its person's instruction.
	c, done = start()
	late := sent(planner("SEND late: too late"))
	if late.Delivered != store.DeliveredSteered || late.RunID != c.ID {
		t.Fatalf("task:message = %+v, want steered into %s", late, c.ID)
	}
	if run = finish(c, done); run.Status != "succeeded" || deref(run.Report) != "answered" {
		t.Fatalf("run = %s %q", run.Status, deref(run.Report))
	}
	waitEvent(t, s, c.ID, "orbit.steer_withdrawn", `"confirmed": true`)
	if ids, triggers := queuedHelper(); len(ids) != 1 || triggers[0] != "task" {
		t.Fatalf("the withdrawn steer woke %v %v, want one task run", ids, triggers)
	}
}

// waitEvent polls until the run has an event of kind whose payload holds text.
func waitEvent(t *testing.T, s *store.Store, runID, kind, text string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM run_events WHERE run_id = $1 AND kind = $2 AND payload::text LIKE '%' || $3 || '%'`,
			runID, kind, text).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	rows, _ := s.Pool.Query(ctx, `SELECT kind, left(payload::text, 400) FROM run_events WHERE run_id = $1 ORDER BY seq`, runID)
	for rows.Next() {
		var k, p string
		_ = rows.Scan(&k, &p)
		t.Log(k, p)
	}
	t.Fatalf("run %s: no %s event with %q", runID, kind, text)
}
