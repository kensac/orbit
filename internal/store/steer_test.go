package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestASteerGoesToTheRunningRunOrIsQueued(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	conv := defaultConv(t, s, "a")
	post := func(body string, wake, steerable bool, delivery string) store.PostResult {
		t.Helper()
		r, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: body, Wake: wake,
			Delivery: delivery, Steerable: steerable})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	queued := func() int {
		t.Helper()
		var n int
		if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE conversation_id = $1 AND status = 'queued'`, conv).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Nothing running: a steer is queued, and wakes a run as asked.
	first := post("first", true, true, store.DeliverySteer)
	if first.Delivered != store.DeliveredQueued || first.RunID == "" {
		t.Fatalf("a steer with nothing running = %+v", first)
	}
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil || c.ID != first.RunID {
		t.Fatalf("claim = %+v %v", c, err)
	}
	if rc, err := s.StartRunContext(ctx, c.ID, "a"); err != nil || len(rc.Messages) != 1 {
		t.Fatalf("run context = %+v %v", rc, err)
	}

	// Running: handed to that run, no run woken.
	also := post("also this", true, true, store.DeliverySteer)
	if also.Delivered != store.DeliveredSteered || also.RunID != c.ID || queued() != 0 {
		t.Fatalf("a steer while a run works = %+v, %d queued", also, queued())
	}
	// Not a chat agent, or a queued message: queued as before.
	if r := post("not steerable", false, false, store.DeliverySteer); r.Delivered != store.DeliveredQueued || r.RunID != "" {
		t.Fatalf("a steer to an agent that cannot take one = %+v", r)
	}
	if r := post("plain", false, true, ""); r.Delivered != store.DeliveredQueued {
		t.Fatalf("a queued message = %+v", r)
	}

	pending, err := s.PendingSteers(ctx, c.ID)
	if err != nil || len(pending) != 1 || pending[0].ID != also.MessageID {
		t.Fatalf("pending steers = %+v %v", pending, err)
	}
	if err := s.MarkSteered(ctx, c.ID, c.Attempt, also.MessageID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkSteered(ctx, c.ID, c.Attempt, also.MessageID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a steer taken twice: %v", err)
	}
	page, err := s.ConversationThread(ctx, conv, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Items {
		if m := it.Message; m != nil && m.ID == also.MessageID && (m.SteeredInto == nil || *m.SteeredInto != c.ID || m.RunID == nil || *m.RunID != c.ID) {
			t.Fatalf("the thread's steered message = %+v", m)
		}
	}

	// A steer the turn did not take: the run's end wakes a run for it.
	late := post("late", false, true, store.DeliverySteer)
	if late.Delivered != store.DeliveredSteered {
		t.Fatalf("late = %+v", late)
	}
	if err := s.FinishRun(ctx, c.ID, store.RunResult{Attempt: c.Attempt, Status: store.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if queued() != 1 {
		t.Fatalf("queued after the run = %d, want one for the steer it did not take", queued())
	}
	next, err := s.ClaimRun(ctx, "w")
	if err != nil || next == nil || next.Trigger != "message" {
		t.Fatalf("next = %+v %v", next, err)
	}
	rc, err := s.StartRunContext(ctx, next.ID, "a")
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for _, m := range rc.Messages {
		bodies = append(bodies, m.Body)
	}
	if len(bodies) != 3 || !slices.Contains(bodies, "late") {
		t.Fatalf("the next run reads %v, want the queued ones and the late steer", bodies)
	}
	if err := s.FinishRun(ctx, next.ID, store.RunResult{Attempt: next.Attempt, Status: store.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	if queued() != 0 {
		t.Fatal("a run with nothing left to answer woke another")
	}

	// A person asked the running run to stop: a steer is queued.
	r := post("stopping", true, true, "")
	c, _ = s.ClaimRun(ctx, "w")
	if c == nil || c.ID != r.RunID {
		t.Fatalf("claim = %+v", c)
	}
	if _, err := s.CancelRun(ctx, c.ID, "me"); err != nil {
		t.Fatal(err)
	}
	if r := post("too late", false, true, store.DeliverySteer); r.Delivered != store.DeliveredQueued {
		t.Fatalf("a steer to a stopping run = %+v", r)
	}
}

// A parent's task:message steers the task's running run only for a chat agent that nobody asked to stop; otherwise it is queued. A parent's steer the turn did not take wakes a task run.
func TestATaskMessageSteersOnlyAChatAgentsRunningRun(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "p", "w")
	parent := manual(t, s, "p")
	pc, err := s.ClaimRun(ctx, "x")
	if err != nil || pc == nil || pc.ID != parent {
		t.Fatalf("claim = %+v %v", pc, err)
	}
	act, err := s.StartActionNow(ctx, parent, pc.Attempt, "p", store.ProposedAction{Type: "delegate", Params: []byte(`{}`), Rule: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	made, err := s.Delegate(ctx, store.Delegation{ActionID: act, RunID: parent, Tasks: []store.NewTask{{Key: "k", Agent: "w", Brief: "do it"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileTasks(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	wc, err := s.ClaimRun(ctx, "x")
	if err != nil || wc == nil || wc.Agent != "w" {
		t.Fatalf("claim = %+v %v", wc, err)
	}
	chat := true
	send := func(body string) store.TaskMessaged {
		t.Helper()
		r, err := s.MessageTask(ctx, store.TaskMessage{FromRun: parent, TaskID: made[0].ID, Body: body,
			Steerable: func(agent string) bool { return agent == "w" && chat }})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	if r := send("steer me"); r.Delivered != store.DeliveredSteered || r.RunID != wc.ID {
		t.Fatalf("to a chat agent's running run = %+v", r)
	}
	chat = false
	not := send("not a chat agent")
	if not.Delivered != store.DeliveredQueued || not.RunID == "" || not.RunID == wc.ID {
		t.Fatalf("to an agent that cannot take one = %+v", not)
	}
	// Its queued run is set aside, so the next message has none to join.
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, not.RunID); err != nil {
		t.Fatal(err)
	}
	chat = true
	if _, err := s.CancelRun(ctx, wc.ID, "me"); err != nil {
		t.Fatal(err)
	}
	if r := send("stopping"); r.Delivered != store.DeliveredQueued || r.RunID == "" {
		t.Fatalf("to a run asked to stop = %+v", r)
	}
	// So is that one, so the run's end finds nothing queued to read the steer.
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET status = 'cancelled' WHERE agent = 'w' AND status = 'queued'`); err != nil {
		t.Fatal(err)
	}

	// The steer still waiting when the run ends wakes a task run, not a person's.
	if err := s.FinishRun(ctx, wc.ID, store.RunResult{Attempt: wc.Attempt, Status: store.StatusSucceeded}); err != nil {
		t.Fatal(err)
	}
	var trigger string
	if err := s.Pool.QueryRow(ctx, `SELECT trigger FROM runs WHERE agent = 'w' AND status = 'queued'`).Scan(&trigger); err != nil || trigger != "task" {
		t.Fatalf("the woken run's trigger = %q %v, want task", trigger, err)
	}
}
