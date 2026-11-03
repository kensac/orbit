package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

// twin is a second orbit instance: another engine on the same database and
// the same data volume, with live processes of its own.
func twin(t *testing.T, e *engine.Engine) *engine.Engine {
	t.Helper()
	e2 := &engine.Engine{Store: e.Store, DataDir: e.DataDir, Env: e.Env, Client: e.Client, Secret: e.Secret,
		NewRunner: e.NewRunner, Log: e.Log, HeartbeatEvery: e.HeartbeatEvery}
	if e.Live != nil {
		e2.Live = runner.NewLive(runner.MaxLive)
	}
	if err := e2.SetConfig(e.Current().Config); err != nil {
		t.Fatal(err)
	}
	return e2
}

// interrupt executes c on e until the runner is working, then stops it as
// a drain's cap does, and waits for the hand-back.
func interrupt(t *testing.T, e *engine.Engine, fr *fakeRunner, c *store.ClaimedRun) {
	t.Helper()
	fr.block, fr.started = make(chan struct{}), make(chan struct{})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error)
	go func() { done <- e.Execute(runCtx, c) }()
	select {
	case <-fr.started:
	case err := <-done:
		t.Fatalf("finished before the runner started: %v", err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	fr.block = nil
}

func attempts(t *testing.T, s *store.Store, run string) []store.Attempt {
	t.Helper()
	a, err := s.ListAttempts(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAnInterruptedRunContinuesItsSessionOnTheOtherInstance(t *testing.T) {
	fr := &fakeRunner{output: goodOutput, savedCost: 0.1}
	old, s, h := harness(t, fr)
	c := claim(t, old, s)
	interrupt(t, old, fr, c)
	first := fr.got

	run, _ := s.GetRun(ctx, c.ID)
	at := attempts(t, s, c.ID)
	if run.Status != "queued" || len(at) != 1 || *at[0].Outcome != "handed_back" || *at[0].SessionID != "session-1" ||
		at[0].CostUSD == nil || *at[0].CostUSD != 0.1 {
		t.Fatalf("after the hand-back: run %s, attempts %+v", run.Status, at[0])
	}
	// The interrupted attempt's scratch stays for the next one.
	if _, err := os.Stat(first.Workdir); err != nil {
		t.Fatalf("the handed-back attempt's workdir is gone: %v", err)
	}

	// The new instance claims it and continues the session it left.
	ready := twin(t, old)
	c2, err := s.ClaimRun(ctx, "new/0")
	if err != nil || c2 == nil || c2.ID != c.ID || c2.Attempt != 2 {
		t.Fatalf("claim = %+v %v", c2, err)
	}
	if err := ready.Execute(ctx, c2); err != nil {
		t.Fatal(err)
	}
	got := fr.got
	if got.Session == nil || got.Session.Resume != "session-1" || got.Workdir != first.Workdir {
		t.Fatalf("attempt 2 resumed %+v in %s, want session-1 in %s", got.Session, got.Workdir, first.Workdir)
	}
	if got.Session.BaseCostUSD != 0.1 {
		t.Fatalf("base = %v, want the interrupted attempt's saved total", got.Session.BaseCostUSD)
	}
	if !strings.Contains(got.Prompt, "previous turn was interrupted") || strings.Contains(got.Prompt, "# Charter") {
		t.Fatalf("a continued attempt is told it was interrupted, not the run again:\n%s", got.Prompt)
	}
	// It continues on exactly the inputs the interrupted attempt read, at
	// the same paths.
	if len(got.Inputs) != 1 || got.Inputs[0].Path != first.Inputs[0].Path ||
		string(got.Inputs[0].Content) != string(first.Inputs[0].Content) {
		t.Fatalf("inputs = %+v, want %+v", got.Inputs, first.Inputs)
	}

	run, _ = s.GetRun(ctx, c.ID)
	at = attempts(t, s, c.ID)
	if run.Status != "succeeded" || len(at) != 2 || at[1].RecoveredFrom == nil || *at[1].RecoveredFrom != 1 ||
		*at[1].Outcome != "succeeded" {
		t.Fatalf("run %s, attempts %+v %+v", run.Status, at[0], at[1])
	}
	// The run's cost is both attempts': 0.1 interrupted, 0.25 to finish.
	if run.CostUSD == nil || *run.CostUSD != 0.35 {
		t.Fatalf("run cost = %v, want 0.35", run.CostUSD)
	}
	if _, err := os.Stat(first.Workdir); err == nil {
		t.Fatal("the finished run's workdir is left behind")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.messages) != 2 {
		t.Fatalf("notifications = %d, want the report and one approval, once", len(h.messages))
	}
}

func TestAnInterruptedRunWithoutItsTranscriptStartsOverAndSaysWhy(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harness(t, fr)
	c := claim(t, e, s)
	interrupt(t, e, fr, c)
	// A /data that is not shared between instances, or was wiped.
	if err := os.RemoveAll(filepath.Join(fr.got.Workdir, ".home")); err != nil {
		t.Fatal(err)
	}
	c2, _ := s.ClaimRun(ctx, "w")
	if err := e.Execute(ctx, c2); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session.Resume != "" || !strings.Contains(fr.got.Prompt, "# Charter") {
		t.Fatalf("started over? resume %q", fr.got.Session.Resume)
	}
	at := attempts(t, s, c.ID)
	if at[1].RecoveredFrom != nil || at[1].RecoverSkipped == nil || !strings.Contains(*at[1].RecoverSkipped, "transcript is gone") {
		t.Fatalf("attempt 2 = %+v", at[1])
	}
}

func TestASessionAgentContinuesTheInterruptedForkNotTheRunBefore(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, sessionSpec)
	first := claim(t, e, s)
	if err := e.Execute(ctx, first); err != nil {
		t.Fatal(err)
	}
	// The second run forks session-1 into session-2 and is interrupted.
	c := claim(t, e, s)
	interrupt(t, e, fr, c)
	if fr.got.Session.Resume != "session-1" {
		t.Fatalf("second run resumed %q", fr.got.Session.Resume)
	}
	c2, _ := s.ClaimRun(ctx, "w")
	if err := twin(t, e).Execute(ctx, c2); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session.Resume != "session-2" {
		t.Fatalf("recovery resumed %q, want the interrupted fork session-2", fr.got.Session.Resume)
	}
	run, _ := s.GetRun(ctx, c.ID)
	if run.Status != "succeeded" || *run.SessionMode != "resumed" || *run.SessionParent != "session-2" {
		t.Fatalf("run = %s %v %v", run.Status, *run.SessionMode, *run.SessionParent)
	}
	// The next run continues the recovered run's own session.
	third := claim(t, e, s)
	if err := e.Execute(ctx, third); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session.Resume != "session-3" {
		t.Fatalf("the run after resumed %q, want session-3", fr.got.Session.Resume)
	}
}

func TestAnAttemptThatLostItsClaimStopsAndWritesNothing(t *testing.T) {
	fr := &fakeRunner{output: goodOutput, block: make(chan struct{}), started: make(chan struct{})}
	e, s, h := harness(t, fr)
	e.HeartbeatEvery = 20 * time.Millisecond
	c := claim(t, e, s)
	done := make(chan error)
	go func() { done <- e.Execute(ctx, c) }()
	<-fr.started
	// Another instance's reaper took the run (this one's heartbeats did
	// not land for StaleAfter), and another worker claimed it. A beat can
	// land between aging the heartbeat and the reap, so age and reap until
	// the reap takes it.
	for reaped := false; !reaped; {
		if _, err := s.Pool.Exec(ctx, `UPDATE runs SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, c.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReapStale(ctx, time.Minute); err != nil {
			t.Fatal(err)
		}
		r, _ := s.GetRun(ctx, c.ID)
		reaped = r.Status == "queued"
	}
	c2, _ := s.ClaimRun(ctx, "other/0")
	if c2 == nil || c2.Attempt != 2 {
		t.Fatalf("reclaim = %+v", c2)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the attempt kept working after losing its claim")
	}
	run, _ := s.GetRun(ctx, c.ID)
	if run.Status != "running" || run.Attempt != 2 {
		t.Fatalf("run = %s attempt %d, want attempt 2 still running", run.Status, run.Attempt)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.messages) != 0 {
		t.Fatalf("a superseded attempt notified: %v", h.messages)
	}
}

func TestAMessageOnTheOtherInstanceResumesTheConversationsSession(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	a, s, _ := harnessWith(t, fr, "session: {resume: true, idle: 1h, live: 10m}\n")
	a.Live = runner.NewLive(runner.MaxLive)
	b := twin(t, a)
	sha := a.Current().Config.Agents["finance"].SHA
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: sha, Body: "how is October?", Wake: true}); err != nil {
		t.Fatal(err)
	}
	c, _ := s.ClaimRun(ctx, "a/0")
	if err := a.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session.Live != a.Live || !fr.got.Session.Open {
		t.Fatal("instance a should hold the conversation's live process")
	}
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "finance", SpecSHA: sha, Body: "and September?", Wake: true}); err != nil {
		t.Fatal(err)
	}
	c2, _ := s.ClaimRun(ctx, "b/0")
	if err := b.Execute(ctx, c2); err != nil {
		t.Fatal(err)
	}
	// Instance b has no process for it, so it resumes the session from the
	// shared volume: continued, not started over.
	if fr.got.Session.Resume != "session-1" || fr.got.Session.Live != b.Live || fr.got.Session.Key != c.ConversationID {
		t.Fatalf("on b: resume %q key %q", fr.got.Session.Resume, fr.got.Session.Key)
	}
}

func TestAStalledTurnHandsTheRunBackToContinueItsSession(t *testing.T) {
	fr := &fakeRunner{output: goodOutput, stalls: 1}
	e, s, _ := harness(t, fr)
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	first := fr.got
	if first.Stall == nil || first.Stall.After != 10*time.Minute {
		t.Fatalf("stall = %+v, want the default watchdog", first.Stall)
	}
	if open, err := first.Stall.ToolOpen(ctx); err != nil || open {
		t.Fatalf("tool open = %v %v", open, err)
	}
	run, _ := s.GetRun(ctx, c.ID)
	at := attempts(t, s, c.ID)
	if run.Status != "queued" || len(at) != 1 || *at[0].Outcome != "handed_back" || *at[0].SessionID != "session-1" {
		t.Fatalf("after the stall: run %s, attempts %+v", run.Status, at)
	}
	if _, err := os.Stat(first.Workdir); err != nil {
		t.Fatalf("the stalled attempt's workdir is gone: %v", err)
	}

	c2, err := s.ClaimRun(ctx, "test")
	if err != nil || c2 == nil || c2.ID != c.ID {
		t.Fatalf("claim = %+v %v", c2, err)
	}
	if err := e.Execute(ctx, c2); err != nil {
		t.Fatal(err)
	}
	got := fr.got
	if c2.Attempt != 2 || got.Session == nil || got.Session.Resume != "session-1" ||
		!strings.Contains(got.Prompt, "orbit interrupted it") || strings.Contains(got.Prompt, "restarted") {
		t.Fatalf("attempt %d resumed %+v with:\n%s", c2.Attempt, got.Session, got.Prompt)
	}
	if run, _ = s.GetRun(ctx, c.ID); run.Status != "succeeded" {
		t.Fatalf("run %s", run.Status)
	}
}

func TestARunThatStallsOnEveryAttemptFails(t *testing.T) {
	fr := &fakeRunner{output: goodOutput, stalls: store.MaxAttempts}
	e, s, _ := harness(t, fr)
	c := claim(t, e, s)
	id := c.ID
	for i := 1; i <= store.MaxAttempts; i++ {
		if i > 1 {
			var err error
			if c, err = s.ClaimRun(ctx, "test"); err != nil || c == nil || c.ID != id {
				t.Fatalf("claim %d = %+v %v", i, c, err)
			}
		}
		if err := e.Execute(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	run, _ := s.GetRun(ctx, id)
	if run.Status != "failed" || run.Error == nil || !strings.Contains(*run.Error, "stalled") || !strings.Contains(*run.Error, "attempt 3 of 3") {
		t.Fatalf("run %s: %q", run.Status, deref(run.Error))
	}
}
