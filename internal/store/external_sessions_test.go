package store_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// Synthetic sessions only: nothing here is from a real transcript.
const (
	extA = "11111111-1111-4111-8111-111111111111"
	extB = "22222222-2222-4222-8222-222222222222"
)

func extLine(typ, text string, at time.Time) store.SessionLine {
	c := fmt.Sprintf(`{"message":{"content":%q,"role":%q},"timestamp":%q,"type":%q}`, text, typ, at.Format(time.RFC3339), typ)
	l := store.SessionLine{Content: []byte(c), Type: typ, At: &at, Cwd: "/home/p/widget"}
	if typ == "user" {
		l.Prompt = text
	}
	return l
}

func hex64(c byte) string { return strings.Repeat(string(c), 64) }

func TestImportSessionsAppendsFoldsAndConflicts(t *testing.T) {
	s := testdb.New(t)
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	l1, l2, l3 := extLine("user", "Why is the widget flaky?", t0), extLine("assistant", "The widget races its timer.", t0.Add(time.Minute)),
		extLine("user", "Then fix the timer.", t0.Add(2*time.Minute))

	// Dry run: reported, nothing written.
	res, err := s.ImportSessions(ctx, "claude-code", []store.SessionChunk{{SessionID: extA, ToOffset: 100, ToPrefix: hex64('a'), Lines: []store.SessionLine{l1, l2}}}, true)
	if err != nil || res[0].Outcome != store.SessionCreated || res[0].ID != "" || res[0].Added != 2 {
		t.Fatalf("dry run = %+v %v", res, err)
	}
	if p, _ := s.ListExternalSessions(ctx, store.SessionFilter{}); len(p.Sessions) != 0 {
		t.Fatalf("a dry run wrote %v", p.Sessions)
	}

	res, err = s.ImportSessions(ctx, "claude-code", []store.SessionChunk{{SessionID: extA, Repo: "someone/widget", ToOffset: 100, ToPrefix: hex64('a'), Lines: []store.SessionLine{l1, l2}}}, false)
	if err != nil || res[0].Outcome != store.SessionCreated || res[0].New != 2 || res[0].Lines != 2 {
		t.Fatalf("create = %+v %v", res, err)
	}
	id := res[0].ID
	// The same state again, with nothing new: unchanged. From a stale offset: a conflict, naming where orbit is.
	res, err = s.ImportSessions(ctx, "claude-code", []store.SessionChunk{
		{SessionID: extA, FromOffset: 100, FromPrefix: hex64('a'), ToOffset: 100, ToPrefix: hex64('a')},
		{SessionID: extA, FromOffset: 50, FromPrefix: hex64('c'), ToOffset: 150, ToPrefix: hex64('d'), Lines: []store.SessionLine{l3}},
	}, false)
	if err != nil || res[0].Outcome != store.SessionUnchanged || res[1].Outcome != store.SessionConflict || !strings.Contains(res[1].Error, "offset 100") {
		t.Fatalf("unchanged and stale = %+v %v", res, err)
	}
	res, err = s.ImportSessions(ctx, "claude-code", []store.SessionChunk{{SessionID: extA, FromOffset: 100, FromPrefix: hex64('a'), ToOffset: 150, ToPrefix: hex64('b'), Lines: []store.SessionLine{l3}}}, false)
	if err != nil || res[0].Outcome != store.SessionAppended || res[0].Lines != 3 || res[0].Offset != 150 {
		t.Fatalf("append = %+v %v", res, err)
	}

	// A fork repeats its parent's lines: they are stored once.
	res, err = s.ImportSessions(ctx, "claude-code", []store.SessionChunk{{SessionID: extB, ToOffset: 160, ToPrefix: hex64('e'),
		Lines: []store.SessionLine{l1, l2, l3, extLine("user", "And the docs.", t0.Add(time.Hour))}}}, false)
	if err != nil || res[0].Outcome != store.SessionCreated || res[0].Added != 4 || res[0].New != 1 {
		t.Fatalf("fork = %+v %v", res, err)
	}

	e, err := s.GetExternalSession(ctx, id)
	if err != nil || e.Lines != 3 || e.OffsetBytes != 150 || e.PrefixSHA256 != hex64('b') || *e.FirstPrompt != "Why is the widget flaky?" ||
		*e.Repo != "someone/widget" || *e.Cwd != "/home/p/widget" || !e.LastAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("session = %+v %v", e, err)
	}
	chain := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	for _, l := range []store.SessionLine{l1, l2, l3} {
		chain = store.ChainSHA256(chain, store.LineDigest(l.Content))
	}
	if e.ContentSHA256 != chain {
		t.Fatalf("content hash = %s, want %s", e.ContentSHA256, chain)
	}
	lines, err := s.ExternalSessionLines(ctx, id, 1, 10)
	if err != nil || len(lines) != 2 || lines[0].N != 2 || lines[1].Content != string(l3.Content) {
		t.Fatalf("lines = %+v %v", lines, err)
	}

	// A rewrite is a new generation; the old lines stay stored.
	res, err = s.ImportSessions(ctx, "claude-code", []store.SessionChunk{{SessionID: extA, Rewrite: true, ToOffset: 40, ToPrefix: hex64('f'), Lines: []store.SessionLine{l2}}}, false)
	if err != nil || res[0].Outcome != store.SessionRewritten || res[0].Generation != 2 || res[0].Lines != 1 {
		t.Fatalf("rewrite = %+v %v", res, err)
	}
	if lines, _ := s.ExternalSessionLines(ctx, id, 0, 0); len(lines) != 1 || lines[0].Content != string(l2.Content) {
		t.Fatalf("lines after rewrite = %+v", lines)
	}
	var kept int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM external_session_lines WHERE external_session_id = $1`, id).Scan(&kept); err != nil || kept != 4 {
		t.Fatalf("lines kept = %d %v", kept, err)
	}

	// The search index has each prose line once, under the first session that had it.
	page, err := s.Search(ctx, store.SearchQuery{Q: "widget timer", Kinds: []string{"session"}})
	if err != nil || page.Total != 1 || page.Results[0].Agent != nil || *page.Results[0].ExternalSessionID != id || *page.Results[0].Seq != 2 {
		t.Fatalf("search = %+v %v", page, err)
	}
	if page, _ := s.Search(ctx, store.SearchQuery{Q: "docs", Agent: "finance"}); page.Total != 0 {
		t.Fatalf("an agent's search finds imported sessions: %+v", page)
	}

	if _, err := s.ImportSessions(ctx, "claude-code", []store.SessionChunk{{SessionID: "nope", ToOffset: 1, ToPrefix: hex64('a')}}, false); err == nil {
		t.Fatal("a session id that is not a UUID was accepted")
	}
	if res, _ := s.ImportSessions(ctx, "claude-code", []store.SessionChunk{{SessionID: "33333333-3333-4333-8333-333333333333", FromOffset: 9, FromPrefix: hex64('a'), ToOffset: 10, ToPrefix: hex64('a')}}, false); res[0].Outcome != store.SessionConflict {
		t.Fatalf("an unknown session continued = %+v", res)
	}
}

func TestListExternalSessionsPages(t *testing.T) {
	s := testdb.New(t)
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	var chunks []store.SessionChunk
	for i := range 3 {
		id := fmt.Sprintf("4444444%d-4444-4444-8444-444444444444", i)
		chunks = append(chunks, store.SessionChunk{SessionID: id, ToOffset: 10, ToPrefix: hex64('a'),
			Lines: []store.SessionLine{extLine("user", fmt.Sprintf("prompt %d", i), t0.Add(time.Duration(i)*time.Hour))}})
	}
	if _, err := s.ImportSessions(ctx, "claude-code", chunks, false); err != nil {
		t.Fatal(err)
	}
	p, err := s.ListExternalSessions(ctx, store.SessionFilter{Limit: 2})
	if err != nil || len(p.Sessions) != 2 || !p.HasMore || *p.Sessions[0].FirstPrompt != "prompt 2" {
		t.Fatalf("page 1 = %+v %v", p, err)
	}
	c, _ := store.ParseConversationCursor(p.NextBefore)
	p, err = s.ListExternalSessions(ctx, store.SessionFilter{Limit: 2, Before: c})
	if err != nil || len(p.Sessions) != 1 || p.HasMore || *p.Sessions[0].FirstPrompt != "prompt 0" {
		t.Fatalf("page 2 = %+v %v", p, err)
	}
}
