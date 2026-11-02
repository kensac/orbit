package store_test

import (
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// finish claims and succeeds the oldest queued run with outcome o.
func finishWith(t *testing.T, s *store.Store, o store.Outcome) string {
	t.Helper()
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if _, err := s.RecordOutcome(ctx, c.ID, c.Agent, store.RunResult{Attempt: c.Attempt, Status: "succeeded", Summary: "ok"}, o); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

func TestEntryWritesApplyWithTheOutcome(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	old, _ := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, EntryFields: fields("user", "Lives in Boston", "Boston.")}, person())
	edited, _ := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeAgent, Owner: "a"}, EntryFields: fields("project", "Rent", "Rent is $2,000.")}, person())
	// A person edits it after the run read version 1.
	body := "Rent is $2,100."
	if _, err := s.UpdateEntry(ctx, edited.ID, 1, store.EntryEdit{Body: &body}, person()); err != nil {
		t.Fatal(err)
	}
	manual(t, s, "a")
	stale := "Rent is $1,900."
	writes := []*store.EntryWrite{
		{Op: "add", New: store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, Key: "lives-in-new-york",
			EntryFields: fields("user", "Lives in New York", "New York.")}, SupersededByIndex: -1, Note: "said so"},
		{Op: "retire", ID: old.ID, Expect: 1, SupersededByIndex: 0, Note: "moved"},
		{Op: "add", New: store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, Key: "lives-in-new-york",
			EntryFields: fields("user", "Lives in New York", "dup")}, SupersededByIndex: -1},
		{Op: "update", ID: edited.ID, Expect: 1, Edit: store.EntryEdit{Body: &stale}, SupersededByIndex: -1},
		{Op: "add", New: store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, Key: "bad", EntryFields: fields("opinion", "Bad", "x")}, SupersededByIndex: -1},
	}
	runID := finishWith(t, s, store.Outcome{Entries: writes})
	want := []string{"written", "written", "refused", "refused", "refused"}
	for i, w := range writes {
		if w.Result.Outcome != want[i] {
			t.Fatalf("write %d = %+v, want %s", i, w.Result, want[i])
		}
	}
	got, _, _ := s.GetEntry(ctx, old.ID)
	if got.Status != "retired" || got.SupersededBy == nil || *got.SupersededBy != writes[0].Result.ID {
		t.Fatalf("retired = %+v", got)
	}
	added, _, _ := s.GetEntry(ctx, writes[0].Result.ID)
	if added.Source != store.SourceLearned || added.SourceRunID == nil || *added.SourceRunID != runID || added.SourceConversationID == nil || added.Note != "said so" {
		t.Fatalf("added = %+v", added)
	}
	if e, _, _ := s.GetEntry(ctx, edited.ID); e.Body != body {
		t.Fatalf("a stale write overwrote a person's edit: %q", e.Body)
	}
	byRun, _ := s.ListEntries(ctx, store.EntryFilter{RunID: runID, Status: "all"})
	if len(byRun) != 2 {
		t.Fatalf("entries by run = %d, want 2", len(byRun))
	}
}

func TestLearningDueAndOnce(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	res, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: "Never count transfers.", Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	// Not due while its run is queued.
	if due, _ := s.LearningDue(ctx, "a", time.Millisecond); len(due) != 0 {
		t.Fatalf("due with a queued run: %v", due)
	}
	finishWith(t, s, store.Outcome{})
	if due, _ := s.LearningDue(ctx, "a", time.Hour); len(due) != 0 {
		t.Fatalf("due before the idle: %v", due)
	}
	time.Sleep(20 * time.Millisecond)
	due, err := s.LearningDue(ctx, "a", 10*time.Millisecond)
	if err != nil || len(due) != 1 {
		t.Fatalf("due = %v, %v", due, err)
	}
	id, err := s.EnqueueLearning(ctx, "a", "sha-a", due[0].ConversationID, due[0].Through)
	if err != nil || id == "" {
		t.Fatalf("enqueue = %q, %v", id, err)
	}
	if again, _ := s.EnqueueLearning(ctx, "a", "sha-a", due[0].ConversationID, due[0].Through); again != "" {
		t.Fatal("a second learning run for the same point was enqueued")
	}
	if due, _ := s.LearningDue(ctx, "a", time.Millisecond); len(due) != 0 {
		t.Fatalf("due while learning is queued: %v", due)
	}
	// The learning run is in the agent's learning conversation, not the one it reads.
	var runConv string
	if err := s.Pool.QueryRow(ctx, `SELECT conversation_id::text FROM runs WHERE id = $1`, id).Scan(&runConv); err != nil {
		t.Fatal(err)
	}
	convs, _ := s.ListConversations(ctx, "a", true, nil, 10)
	if len(convs.Conversations) != 1 || convs.Conversations[0].ID != runConv || *convs.Conversations[0].Purpose != "learning" {
		t.Fatalf("learning conversation = %+v, run in %s", convs.Conversations, runConv)
	}
	page, _ := s.ConversationThread(ctx, due[0].ConversationID, nil, 50)
	for _, it := range page.Items {
		if it.Run != nil && it.Run.Trigger == store.TriggerLearn {
			t.Fatal("the learning run shows in the conversation it read")
		}
	}
	w, err := s.LearningWindowOf(ctx, id)
	if err != nil || w.ConversationID != due[0].ConversationID || w.After != nil {
		t.Fatalf("window = %+v, %v", w, err)
	}
	items, _ := s.LearningItems(ctx, *w, 50)
	if len(items) != 2 || items[1].Body != "Never count transfers." || items[0].Kind != "report" {
		t.Fatalf("items = %+v", items)
	}
	_ = res
	// Once it succeeds, the next window starts after its point.
	finishWith(t, s, store.Outcome{})
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: "Also skip refunds.", ConversationID: due[0].ConversationID}); err != nil {
		t.Fatal(err)
	}
	next, err := s.LearnNow(ctx, due[0].ConversationID, "sha-a")
	if err != nil {
		t.Fatal(err)
	}
	w2, _ := s.LearningWindowOf(ctx, next)
	if w2.After == nil || !w2.After.Equal(w.Through) {
		t.Fatalf("second window = %+v", w2)
	}
	items, _ = s.LearningItems(ctx, *w2, 50)
	// The reply to the first window's last message finished after its point, so it is read again: context, not news.
	if len(items) != 2 || items[0].Body != "Also skip refunds." {
		t.Fatalf("second items = %+v", items)
	}
	if _, err := s.LearnNow(ctx, due[0].ConversationID, "sha-a"); err == nil {
		t.Fatal("learning the same point twice was allowed")
	}
}
