package store_test

import (
	"errors"
	"slices"
	"testing"

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
