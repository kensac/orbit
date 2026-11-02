package store_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestBoardStatePrecedence(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	idle := 30 * time.Minute
	ago := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }
	failed := &store.RunSummary{Status: "failed", FinishedAt: ago(time.Hour)}
	for _, c := range []struct {
		name string
		f    store.BoardFacts
		want string
	}{
		{"an approval outranks a running run", store.BoardFacts{Working: true, Counts: store.BoardCounts{Approvals: 1}, LastActivityAt: now}, store.BoardNeedsYou},
		{"a question", store.BoardFacts{Counts: store.BoardCounts{Questions: 1}, LastActivityAt: *ago(48 * time.Hour)}, store.BoardNeedsYou},
		{"a blocked task", store.BoardFacts{Task: &store.BoardTask{Status: "blocked"}, LastActivityAt: now}, store.BoardNeedsYou},
		{"a run in progress", store.BoardFacts{Working: true, Counts: store.BoardCounts{Unread: 2}, LastActivityAt: now}, store.BoardWorking},
		{"a task asking its parent", store.BoardFacts{Task: &store.BoardTask{Status: "needs_input"}, LastActivityAt: now}, store.BoardWorking},
		{"a failure not yet read", store.BoardFacts{LastRun: failed, ReadThrough: ago(2 * time.Hour), Counts: store.BoardCounts{Unread: 1}, LastActivityAt: *ago(2 * time.Hour)}, store.BoardFailed},
		{"a failure read but recent", store.BoardFacts{LastRun: &store.RunSummary{Status: "failed", FinishedAt: ago(time.Minute)}, ReadThrough: ago(0), LastActivityAt: *ago(time.Minute)}, store.BoardFailed},
		{"a failure read and quiet", store.BoardFacts{LastRun: failed, ReadThrough: ago(0), LastActivityAt: *ago(2 * time.Hour)}, store.BoardIdle},
		{"a reply not read", store.BoardFacts{Counts: store.BoardCounts{Unread: 1}, LastActivityAt: *ago(3 * time.Hour)}, store.BoardDoneUnread},
		{"read and recent", store.BoardFacts{LastActivityAt: *ago(10 * time.Minute)}, store.BoardRead},
		{"read and quiet", store.BoardFacts{LastActivityAt: *ago(31 * time.Minute)}, store.BoardIdle},
	} {
		if got := store.BoardState(c.f, now, idle); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func boardRow(t *testing.T, s *store.Store, conversation string, withIdle bool) *store.BoardRow {
	t.Helper()
	b, err := s.ReadBoard(ctx, 0, withIdle, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range b.Rows {
		if b.Rows[i].ConversationID == conversation {
			return &b.Rows[i]
		}
	}
	return nil
}

func TestBoardFollowsAConversation(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	res, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Body: "How much on **Uber Eats**?", Author: "me", Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	cid := res.ConversationID
	r := boardRow(t, s, cid, false)
	if r == nil || r.State != store.BoardWorking || r.Preview == nil || r.Preview.From != "person" || r.Preview.Text != "How much on **Uber Eats**?" ||
		r.LastRun == nil || r.LastRun.Status != "queued" || r.ReadThrough != nil || r.Agent != "a" {
		t.Fatalf("queued = %+v", r)
	}
	c, _ := s.ClaimRun(ctx, "w")
	if r := boardRow(t, s, cid, false); r.State != store.BoardWorking || r.LastRun.Status != "running" {
		t.Fatalf("running = %+v", r)
	}
	if _, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: 1, Status: "succeeded", Summary: "$212 in October", Report: "long"}, store.Outcome{}); err != nil {
		t.Fatal(err)
	}
	r = boardRow(t, s, cid, false)
	if r.State != store.BoardDoneUnread || r.Counts.Unread != 1 || r.Preview.From != "agent" || r.Preview.Text != "$212 in October" {
		t.Fatalf("finished = %+v", r)
	}
	// The conversation list carries the same last run.
	conv, err := s.GetConversation(ctx, cid)
	if err != nil || conv.LastRun == nil || conv.LastRun.ID != c.ID || conv.LastRun.Status != "succeeded" || conv.LastRun.FinishedAt == nil {
		t.Fatalf("conversation = %+v %v", conv, err)
	}

	// Reading moves the point forward only, never past now.
	at, err := s.MarkRead(ctx, cid, nil)
	if err != nil {
		t.Fatal(err)
	}
	past := at.Add(-time.Hour)
	if back, _ := s.MarkRead(ctx, cid, &past); !back.Equal(at) {
		t.Fatalf("mark back = %v, want %v", back, at)
	}
	future := time.Now().Add(time.Hour)
	if ahead, _ := s.MarkRead(ctx, cid, &future); !ahead.Before(future) {
		t.Fatalf("mark ahead = %v, want clamped to now", ahead)
	}
	if r := boardRow(t, s, cid, false); r.State != store.BoardRead || r.Counts.Unread != 0 || r.ReadThrough == nil {
		t.Fatalf("read = %+v", r)
	}
	if _, err := s.MarkRead(ctx, "00000000-0000-0000-0000-000000000000", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("mark an unknown conversation = %v", err)
	}

	// Quiet past the idle window: left out unless asked for.
	if _, err := s.Pool.Exec(ctx, `UPDATE conversations SET last_activity_at = now() - interval '2 hours' WHERE id = $1`, cid); err != nil {
		t.Fatal(err)
	}
	if r := boardRow(t, s, cid, false); r != nil {
		t.Fatalf("idle row listed: %+v", r)
	}
	if r := boardRow(t, s, cid, true); r == nil || r.State != store.BoardIdle || r.AgeSeconds < 7000 {
		t.Fatalf("idle with idle=1 = %+v", r)
	}

	// A question and an approval need the person.
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: "and November?", Author: "me", Wake: true, ConversationID: cid}); err != nil {
		t.Fatal(err)
	}
	c, _ = s.ClaimRun(ctx, "w")
	if _, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: 1, Status: "succeeded", Report: "Which card?"}, store.Outcome{
		Questions: []string{"Which card?"},
		Actions:   []store.ProposedAction{{Type: "notify", Params: json.RawMessage(`{}`), Reason: "tell you", Rule: "approve"}},
	}); err != nil {
		t.Fatal(err)
	}
	r = boardRow(t, s, cid, false)
	if r.State != store.BoardNeedsYou || r.Counts.Questions != 1 || r.Counts.Approvals != 1 {
		t.Fatalf("waiting = %+v", r)
	}

	// A failure stands until something newer happens.
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: "Visa", Author: "me", Wake: true, ConversationID: cid}); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.ListActions(ctx, "a", "pending", 0)
	for _, x := range pending {
		_ = s.DecideAction(ctx, x.ID, false, "me")
	}
	c, _ = s.ClaimRun(ctx, "w")
	if err := s.FinishRun(ctx, c.ID, store.RunResult{Attempt: 1, Status: "failed", Error: "source: 502"}); err != nil {
		t.Fatal(err)
	}
	if r := boardRow(t, s, cid, false); r.State != store.BoardFailed || r.LastRun.Status != "failed" {
		t.Fatalf("failed = %+v", r)
	}
}

func TestBoardLeavesOutArchivedAndShowsTasks(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	res, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Title: "old"})
	if err != nil {
		t.Fatal(err)
	}
	archived := true
	if err := s.UpdateConversation(ctx, res.ConversationID, store.ConversationUpdate{Archived: &archived}); err != nil {
		t.Fatal(err)
	}
	if r := boardRow(t, s, res.ConversationID, true); r != nil {
		t.Fatalf("archived listed: %+v", r)
	}
}

func TestBoardShowsADelegatedTaskAndItsDecision(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a", "b")
	manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	ids, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{{Type: "delegate", Params: json.RawMessage(`{}`), Rule: "auto"}}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Delegate(ctx, store.Delegation{ActionID: ids[0], RunID: c.ID,
		Tasks: []store.NewTask{{Key: "k", Agent: "b", Brief: "book it", Decision: "Spend $50 on the venue?"}}})
	if err != nil || len(out) != 1 {
		t.Fatalf("delegate = %+v %v", out, err)
	}
	parent, err := s.DefaultConversationID(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	p := boardRow(t, s, parent, false)
	if p == nil || p.State != store.BoardNeedsYou || p.Counts.Decisions != 1 || p.Counts.Approvals != 0 || p.Counts.Tasks != 1 {
		t.Fatalf("parent = %+v", p)
	}
	k := boardRow(t, s, out[0].ConversationID, false)
	if k == nil || k.Task == nil || k.Task.ID != out[0].ID || k.Task.Status != "blocked" || k.Task.ParentConversationID != parent ||
		k.State != store.BoardNeedsYou || k.Agent != "b" || k.Preview == nil || k.Preview.From != "agent" || k.Preview.Text != "book it" {
		t.Fatalf("task = %+v %+v", k, k.Task)
	}
}

func TestPreviewText(t *testing.T) {
	if got := store.PreviewText("# Title\n\n- one\n- two  three"); got != "Title one two three" {
		t.Fatalf("preview = %q", got)
	}
	long := strings.Repeat("word ", 60)
	if got := store.PreviewText(long); len([]rune(got)) > 140 || !strings.HasSuffix(got, "...") {
		t.Fatalf("long preview = %q", got)
	}
}

// settledChanges reads the feed after cur until want reports it has what it
// needs: other transactions in the cluster (a shared test server) hold back
// what is settled, so a test waits rather than assumes.
func settledChanges(t *testing.T, s *store.Store, cur store.ChangeCursor, want func([]store.Change) bool) []store.Change {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		got, err := s.ChangesAfter(ctx, cur, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if want(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("changes = %+v", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestChangesNeverSkipALateCommit(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	other, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Title: "other"})
	if err != nil {
		t.Fatal(err)
	}
	conv := other.ConversationID
	all := settledChanges(t, s, store.ChangeCursor{}, func(c []store.Change) bool {
		return len(c) > 0 && c[len(c)-1].ConversationID != nil && *c[len(c)-1].ConversationID == conv
	})
	head, err := store.ParseChangeCursor(all[len(all)-1].Cursor)
	if err != nil {
		t.Fatal(err)
	}
	// An early transaction writes first and commits last.
	early, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = early.Rollback(ctx) }()
	if _, err := early.Exec(ctx, `UPDATE conversations SET title = 'renamed' WHERE id = $1`, conv); err != nil {
		t.Fatal(err)
	}
	late := manual(t, s, "a")
	time.Sleep(200 * time.Millisecond)
	got, err := s.ChangesAfter(ctx, head, 100)
	if err != nil || len(got) != 0 {
		t.Fatalf("changes while an older transaction is open = %+v %v, want none yet", got, err)
	}
	if err := early.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got = settledChanges(t, s, head, func(c []store.Change) bool { return len(c) == 2 })
	if got[0].Kind != "conversation" || *got[0].ConversationID != conv || got[1].Kind != "run" || *got[1].RunID != late {
		t.Fatalf("changes = %+v, want the rename then the run", got)
	}
	next, err := store.ParseChangeCursor(got[1].Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if more, _ := s.ChangesAfter(ctx, next, 100); len(more) != 0 {
		t.Fatalf("after the last cursor = %+v", more)
	}
	if n, _ := s.ChangesBehind(ctx, head, 1); n != 2 {
		t.Fatalf("behind = %d, want 2 (most+1)", n)
	}
	if h, _ := s.ChangesHead(ctx); h != next {
		t.Fatalf("head = %v, want %v", h, next)
	}
	if _, err := store.ParseChangeCursor("1.2"); err == nil {
		t.Fatal("a journal cursor parsed as a change cursor")
	}
}
