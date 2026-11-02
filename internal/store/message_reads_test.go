package store_test

import (
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// threadMessage is the thread's view of one person's message.
func threadMessage(t *testing.T, s *store.Store, agent, id string) *store.ThreadMessage {
	t.Helper()
	page, err := s.Thread(ctx, agent, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range page.Items {
		if it.Message != nil && it.Message.ID == id {
			return it.Message
		}
	}
	t.Fatalf("message %s not in the thread", id)
	return nil
}

func TestHandBackKeepsWhichAttemptReadAMessage(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	msg, err := s.SendToAgent(ctx, "a", "hello", "me", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m := threadMessage(t, s, "a", msg); m.RunID != nil || len(m.Reads) != 0 {
		t.Fatalf("unread message: run_id %v, reads %+v", m.RunID, m.Reads)
	}
	id := manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	if rc, err := s.StartRunContext(ctx, c.ID, "a"); err != nil || len(rc.Messages) != 1 {
		t.Fatalf("first attempt read %+v %v", rc, err)
	}
	if err := s.HandBack(ctx, id, c.Attempt, nil, nil); err != nil {
		t.Fatal(err)
	}

	// Handed back: waiting again, and the attempt that read it is kept.
	m := threadMessage(t, s, "a", msg)
	if m.RunID != nil {
		t.Fatalf("a handed-back message waits, run_id = %s", *m.RunID)
	}
	if len(m.Reads) != 1 || m.Reads[0].RunID != id || m.Reads[0].Attempt != 1 || m.Reads[0].ReleasedAt == nil {
		t.Fatalf("reads after hand-back = %+v", m.Reads)
	}

	// The next attempt reads it, exactly once.
	c2, _ := s.ClaimRun(ctx, "w")
	if c2 == nil || c2.ID != id || c2.Attempt != 2 {
		t.Fatalf("second claim = %+v", c2)
	}
	rc, err := s.StartRunContext(ctx, c2.ID, "a")
	if err != nil || len(rc.Messages) != 1 || rc.Messages[0].ID != msg {
		t.Fatalf("second attempt read %+v %v", rc, err)
	}
	if again, _ := s.StartRunContext(ctx, c2.ID, "a"); len(again.Messages) != 0 {
		t.Fatal("a held message was delivered twice")
	}
	m = threadMessage(t, s, "a", msg)
	if m.RunID == nil || *m.RunID != id {
		t.Fatalf("held message run_id = %v, want %s", m.RunID, id)
	}
	if len(m.Reads) != 2 || m.Reads[0].Attempt != 1 || m.Reads[0].ReleasedAt == nil ||
		m.Reads[1].Attempt != 2 || m.Reads[1].ReleasedAt != nil {
		t.Fatalf("reads after the second attempt = %+v", m.Reads)
	}
}

func TestFailedRunKeepsItsReadAndTheNextRunTakesIt(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	msg, _ := s.SendToAgent(ctx, "a", "hello", "me", nil)
	first := manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	if _, err := s.StartRunContext(ctx, c.ID, "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, first, store.RunResult{Attempt: c.Attempt, Status: "failed", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	second := manual(t, s, "a")
	c2, _ := s.ClaimRun(ctx, "w")
	// While it waits, the next run's prompt does not show it as history:
	// it shows it as new.
	hist, err := s.Conversation(ctx, "a", second, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range hist {
		if it.Kind == "message" {
			t.Fatalf("a waiting message is history: %+v", it)
		}
	}
	rc, _ := s.StartRunContext(ctx, c2.ID, "a")
	if len(rc.Messages) != 1 || rc.Messages[0].ID != msg {
		t.Fatalf("next run read %+v", rc.Messages)
	}
	m := threadMessage(t, s, "a", msg)
	if m.RunID == nil || *m.RunID != second {
		t.Fatalf("run_id = %v, want the run that holds it", m.RunID)
	}
	if len(m.Reads) != 2 || m.Reads[0].RunID != first || m.Reads[0].ReleasedAt == nil ||
		m.Reads[1].RunID != second || m.Reads[1].ReleasedAt != nil {
		t.Fatalf("reads = %+v", m.Reads)
	}
	if err := s.FinishRun(ctx, second, store.RunResult{Attempt: c2.Attempt, Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	third := manual(t, s, "a")
	hist, _ = s.Conversation(ctx, "a", third, 50)
	var seen int
	for _, it := range hist {
		if it.Kind == "message" && it.RunID == second {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("the read message is history of the run that holds it %d times, want 1: %+v", seen, hist)
	}
}

// A message read by an image older than message_reads (run_id set, no read
// row) stays held, and is handed back by this image's release.
func TestReadByAnOlderImage(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	msg, _ := s.SendToAgent(ctx, "a", "hello", "me", nil)
	id := manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	if _, err := s.Pool.Exec(ctx, `UPDATE messages SET run_id = $1 WHERE id = $2`, id, msg); err != nil {
		t.Fatal(err)
	}
	if rc, _ := s.StartRunContext(ctx, c.ID, "a"); len(rc.Messages) != 0 {
		t.Fatal("a message an older image holds was read again")
	}
	if err := s.HandBack(ctx, id, c.Attempt, nil, nil); err != nil {
		t.Fatal(err)
	}
	if m := threadMessage(t, s, "a", msg); m.RunID != nil || len(m.Reads) != 1 || m.Reads[0].ReleasedAt == nil {
		t.Fatalf("after hand-back: run_id %v, reads %+v", m.RunID, m.Reads)
	}
	c2, _ := s.ClaimRun(ctx, "w")
	if rc, _ := s.StartRunContext(ctx, c2.ID, "a"); len(rc.Messages) != 1 {
		t.Fatalf("next attempt read %+v", rc.Messages)
	}

	// An older image's hand-back clears run_id and leaves the read open;
	// the message waits, and the next read closes that row.
	if _, err := s.Pool.Exec(ctx, `UPDATE messages SET run_id = NULL WHERE id = $1`, msg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET status = 'queued', worker = NULL, started_at = NULL, heartbeat_at = NULL
		WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	c3, _ := s.ClaimRun(ctx, "w")
	if rc, err := s.StartRunContext(ctx, c3.ID, "a"); err != nil || len(rc.Messages) != 1 {
		t.Fatalf("after an older image's hand-back: %+v %v", rc, err)
	}
	if m := threadMessage(t, s, "a", msg); len(m.Reads) != 3 || m.Reads[2].ReleasedAt != nil {
		t.Fatalf("reads = %+v", m.Reads)
	}
}
