package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestDefaultTitle(t *testing.T) {
	for in, want := range map[string]string{
		"How much did I spend on Uber Eats?":     "How much did I spend on Uber Eats?",
		"\n\n## Rent\n\nwas it late?":            "Rent",
		"  > quoted   line\twith  tabs ":         "quoted line with tabs",
		strings.Repeat("word ", 30):              "word word word word word word word word word word word...",
		strings.Repeat("x", 80):                  strings.Repeat("x", 57) + "...",
		"- [ ] check the October statement only": "[ ] check the October statement only",
	} {
		if got := store.DefaultTitle(in); got != want {
			t.Errorf("DefaultTitle(%q) = %q, want %q", in, got, want)
		}
		if got := store.DefaultTitle(in); len([]rune(got)) > 60 {
			t.Errorf("title %q is longer than 60", got)
		}
	}
}

func TestUpgradePutsExistingRowsInTheDefaultConversation(t *testing.T) {
	s := testdb.Empty(t)
	if err := s.MigrateTo(ctx, 3); err != nil {
		t.Fatal(err)
	}
	// Rows as the previous release wrote them.
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO agent_specs (sha, agent, spec) VALUES ('sha-a', 'a', '{}');
		INSERT INTO agents (name, spec_sha, enabled) VALUES ('a', 'sha-a', true);
		INSERT INTO runs (agent, spec_sha, trigger) VALUES ('a', 'sha-a', 'manual');
		INSERT INTO messages (agent, direction, body) VALUES ('a', 'to_agent', 'old');`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	conv := defaultConv(t, s, "a")
	var orphans int
	if err := s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM messages WHERE conversation_id <> $1) +
		(SELECT count(*) FROM runs WHERE conversation_id <> $1)`, conv).Scan(&orphans); err != nil || orphans != 0 {
		t.Fatalf("rows outside the default conversation = %d %v", orphans, err)
	}
	// The previous release, still running during a deploy, writes rows
	// without a conversation: they land in the default one.
	if _, err := s.Pool.Exec(ctx, `INSERT INTO messages (agent, direction, body) VALUES ('a', 'to_agent', 'during deploy')`); err != nil {
		t.Fatal(err)
	}
	page, err := s.Thread(ctx, "a", nil, 0)
	if err != nil || len(page.Items) != 3 {
		t.Fatalf("default thread = %+v %v", page, err)
	}
	c, _ := s.GetConversation(ctx, conv)
	if c.Title != "General" || !c.IsDefault {
		t.Fatalf("default conversation = %+v", c)
	}
}

func TestConversationsAreIndependentThreads(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a", "b")
	general := defaultConv(t, s, "a")
	open := func(body, client string) store.CreateResult {
		t.Helper()
		r, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a", Body: body, Author: "kanishk",
			Wake: true, ClientID: client})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	one := open("Rent: was October late?", "c1")
	if again, replayed, _ := s.CreateConversation(ctx, store.NewConversation{Agent: "a", SpecSHA: "sha-a",
		Body: "Rent: was October late?", Author: "kanishk", Wake: true, ClientID: "c1"}); !replayed || again != one {
		t.Fatalf("retried create = %+v %v", again, replayed)
	}
	two := open("Groceries", "")
	if _, err := s.SendToAgent(ctx, "a", "general", "kanishk", nil); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.GetConversation(ctx, one.ConversationID); c.Title != "Rent: was October late?" || c.IsDefault {
		t.Fatalf("titled from its first message: %+v", c)
	}

	// finish claims the next run, checks its conversation, and records it.
	finish := func(wantConv, session string) (string, []string) {
		t.Helper()
		c, err := s.ClaimRun(ctx, "w")
		if err != nil || c == nil || c.ConversationID != wantConv {
			t.Fatalf("claim %+v %v, want conversation %s", c, err, wantConv)
		}
		rc, err := s.StartRunContext(ctx, c.ID, "a")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Status: "succeeded", Report: "r", SessionID: session},
			store.Outcome{Questions: []string{"which?"}}); err != nil {
			t.Fatal(err)
		}
		var bodies []string
		for _, m := range rc.Messages {
			bodies = append(bodies, m.Body)
		}
		return c.ID, bodies
	}
	oneRun, got := finish(one.ConversationID, "s-one")
	if len(got) != 1 || got[0] != "Rent: was October late?" {
		t.Fatalf("conversation one read %v", got)
	}
	twoRun, got := finish(two.ConversationID, "s-two")
	if len(got) != 1 || got[0] != "Groceries" {
		t.Fatalf("conversation two read %v", got)
	}
	// The general message woke nothing (SendToAgent); a manual run in the
	// default conversation reads it.
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: "sha-a", Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	generalRun, got := finish(general, "s-general")
	if len(got) != 1 || got[0] != "general" {
		t.Fatalf("default conversation read %v", got)
	}

	// Sessions are per conversation.
	for conv, want := range map[string]string{one.ConversationID: "s-one", two.ConversationID: "s-two", general: "s-general"} {
		if p, _ := s.LatestSession(ctx, conv); p == nil || p.SessionID != want {
			t.Fatalf("session of %s = %+v, want %s", conv, p, want)
		}
	}
	// A message in two closes only two's question.
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: "Whole Foods only",
		ConversationID: two.ConversationID}); err != nil {
		t.Fatal(err)
	}
	needs := map[string]bool{}
	page, _ := s.ListConversations(ctx, "a", false, nil, 0)
	for _, c := range page.Conversations {
		needs[c.ID] = c.NeedsInput
	}
	if !needs[one.ConversationID] || needs[two.ConversationID] || !needs[general] || len(page.Conversations) != 3 {
		t.Fatalf("needs_input = %v", needs)
	}
	// Newest activity first: two just got a message.
	if page.Conversations[0].ID != two.ConversationID {
		t.Fatalf("list order = %+v", page.Conversations)
	}

	// The conversation a run is shown is its own.
	convo, _ := s.Conversation(ctx, "a", twoRun, 50)
	for _, it := range convo {
		if it.RunID == oneRun || it.RunID == generalRun || it.Body == "general" || strings.Contains(it.Body, "Rent") {
			t.Fatalf("conversation two shows another conversation: %+v", it)
		}
	}
	// A continuation stays in its run's conversation.
	cid, _, err := s.EnqueueContinuation(ctx, store.Continuation{Agent: "a", FromRun: oneRun, After: time.Minute, Note: "n", MaxChain: 3})
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRun(ctx, cid); r == nil {
		t.Fatal("no continuation")
	}
	th, _ := s.ConversationThread(ctx, one.ConversationID, nil, 0)
	if !hasRun(th, cid) || hasRun(th, twoRun) {
		t.Fatal("the continuation must be in conversation one's thread, and nothing else")
	}

	// A reply must name a message in the same conversation.
	var question string
	for _, it := range th.Items {
		if it.Message != nil && it.Message.From == "agent" {
			question = it.Message.ID
		}
	}
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", Body: "x", ReplyTo: &question,
		ConversationID: two.ConversationID}); !errors.Is(err, store.ErrReplyElsewhere) {
		t.Fatalf("reply across conversations = %v", err)
	}
	// Another agent cannot post into a's conversation.
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "b", Body: "x", ConversationID: one.ConversationID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("post across agents = %v", err)
	}
}

func hasRun(p *store.ThreadPage, id string) bool {
	for _, it := range p.Items {
		if it.Run != nil && it.Run.ID == id {
			return true
		}
	}
	return false
}

func TestArchiveTitleAndPaging(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	var ids []string
	for i := 0; i < 5; i++ {
		r, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "a", Title: "c"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ConversationID)
	}
	// Every conversation exactly once across pages of two.
	seen := map[string]int{}
	var cur *store.ConversationCursor
	for pages := 0; pages < 10; pages++ {
		p, err := s.ListConversations(ctx, "a", false, cur, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range p.Conversations {
			seen[c.ID]++
		}
		if !p.HasMore {
			break
		}
		cur, _ = store.ParseConversationCursor(p.NextBefore)
	}
	if len(seen) != 6 {
		t.Fatalf("seen %d conversations, want 5 and the default", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s seen %d times", id, n)
		}
	}

	yes := true
	if err := s.UpdateConversation(ctx, ids[0], store.ConversationUpdate{Archived: &yes}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.ListConversations(ctx, "a", true, nil, 0); len(p.Conversations) != 1 || p.Conversations[0].ID != ids[0] {
		t.Fatalf("archived list = %+v", p)
	}
	if err := s.UpdateConversation(ctx, defaultConv(t, s, "a"), store.ConversationUpdate{Archived: &yes}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("archive the default = %v", err)
	}
	// A person's title is kept; a message to an archived conversation
	// brings it back.
	title := "  Taxes 2026 "
	_ = s.UpdateConversation(ctx, ids[0], store.ConversationUpdate{Title: &title})
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", Body: "first words", ConversationID: ids[0]}); err != nil {
		t.Fatal(err)
	}
	c, _ := s.GetConversation(ctx, ids[0])
	if c.Archived || c.Title != "Taxes 2026" {
		t.Fatalf("after a message = %+v", c)
	}
	if err := s.UpdateConversation(ctx, "00000000-0000-0000-0000-000000000000", store.ConversationUpdate{Title: &title}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown = %v", err)
	}
}
