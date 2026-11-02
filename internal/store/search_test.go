package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// searchMigration is the migration that creates search_docs.
const searchMigration = 20261102032149

// searchedRun runs one claimed run of agent to success with a transcript of
// events, a report and a memory document.
func searchedRun(t *testing.T, s *store.Store, agent string, events []string, summary, report, memory string) string {
	t.Helper()
	id := manual(t, s, agent)
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil || c.ID != id {
		t.Fatalf("claim: %v %v", c, err)
	}
	if _, err := s.StartRunContext(ctx, id, agent); err != nil {
		t.Fatal(err)
	}
	for i, ev := range events {
		if err := s.AppendEvent(ctx, id, c.Attempt, i+1, "assistant", []byte(ev)); err != nil {
			t.Fatal(err)
		}
	}
	var mem *string
	if memory != "" {
		mem = &memory
	}
	if _, err := s.RecordOutcome(ctx, id, agent, store.RunResult{Attempt: c.Attempt, Status: store.StatusSucceeded,
		Summary: summary, Report: report}, store.Outcome{Memory: mem, Questions: []string{"Should the electricity budget move?"}}); err != nil {
		t.Fatal(err)
	}
	return id
}

const electricityEvents = `{"type":"assistant","message":{"content":[` +
	`{"type":"text","text":"Reading the electricity invoices first."},` +
	`{"type":"tool_use","name":"Grep","input":{"pattern":"kilowatt","path":"inputs"}},` +
	`{"type":"tool_use","name":"StructuredOutput","input":{"summary":"electricity electricity electricity"}}]}}`

func kindsOf(p *store.SearchPage) map[string]int {
	got := map[string]int{}
	for _, r := range p.Results {
		got[r.Kind]++
	}
	return got
}

func TestSearchFindsEveryKindWithHighlights(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "finance", "other")
	if _, err := s.SendToAgent(ctx, "finance", "Why did the electricity bill jump?", "kanishk", nil); err != nil {
		t.Fatal(err)
	}
	run := searchedRun(t, s, "finance", []string{electricityEvents}, "Electricity rose 12%",
		"The **electricity** bill rose because of a rate change.", "Electricity baseline: $80 a month.")
	if _, err := s.SendToAgent(ctx, "other", "nothing about power here", "kanishk", nil); err != nil {
		t.Fatal(err)
	}

	page, err := s.Search(ctx, store.SearchQuery{Q: "electricity"})
	if err != nil {
		t.Fatal(err)
	}
	got := kindsOf(page)
	want := map[string]int{"message": 1, "question": 1, "report": 1, "narration": 1, "memory": 1}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("kind %s: %d results, want %d (all: %v)", k, got[k], n, got)
		}
	}
	if got["tool_call"] != 0 {
		t.Errorf("the StructuredOutput call was indexed: %v", got)
	}
	if page.Total != len(page.Results) {
		t.Errorf("total %d, results %d", page.Total, len(page.Results))
	}
	for _, r := range page.Results {
		if r.Agent == nil || *r.Agent != "finance" || r.ConversationID == nil {
			t.Errorf("result %+v: want finance, in a conversation", r)
		}
		matched := false
		var joined strings.Builder
		for _, p := range r.Parts {
			joined.WriteString(p.Text)
			if p.Match && strings.EqualFold(p.Text, "electricity") {
				matched = true
			}
		}
		if !matched || joined.String() != r.Snippet || strings.ContainsAny(r.Snippet, "\x01\x02") {
			t.Errorf("%s snippet %q parts %+v: want electricity marked, parts joining to the snippet", r.Kind, r.Snippet, r.Parts)
		}
		if r.Kind != "message" && (r.RunID == nil || *r.RunID != run) {
			t.Errorf("%s: run %v, want %s", r.Kind, r.RunID, run)
		}
	}

	tools, err := s.Search(ctx, store.SearchQuery{Q: "kilowatt", Kinds: []string{"tool_call"}})
	if err != nil || len(tools.Results) != 1 || *tools.Results[0].Tool != "Grep" || *tools.Results[0].Seq != 1 {
		t.Fatalf("tool call search = %+v %v", tools, err)
	}
	if p, err := s.Search(ctx, store.SearchQuery{Q: "electricity", Agent: "other"}); err != nil || p.Total != 0 {
		t.Fatalf("other agent's search = %+v %v, want nothing", p, err)
	}
	if p, err := s.Search(ctx, store.SearchQuery{Q: "electricity", RunID: run, Kinds: []string{"report", "narration"}}); err != nil || p.Total != 2 {
		t.Fatalf("run filter = %+v %v, want report and narration", p, err)
	}
	future := time.Now().Add(time.Hour)
	if p, err := s.Search(ctx, store.SearchQuery{Q: "electricity", From: &future}); err != nil || p.Total != 0 {
		t.Fatalf("from the future = %+v %v", p, err)
	}
	if p, err := s.Search(ctx, store.SearchQuery{Q: "electricity -invoices"}); err != nil || kindsOf(p)["narration"] != 0 {
		t.Fatalf("excluded word still matched the narration: %+v %v", p, err)
	}
	if _, err := s.Search(ctx, store.SearchQuery{Q: "x", Kinds: []string{"bogus"}}); err == nil {
		t.Fatal("an unknown kind was accepted")
	}
	if _, err := s.Search(ctx, store.SearchQuery{Q: "  "}); err == nil {
		t.Fatal("an empty query was accepted")
	}
	if p, err := s.Search(ctx, store.SearchQuery{Q: "electricity", Offset: 50}); err != nil || len(p.Results) != 0 || p.Total != page.Total {
		t.Fatalf("past the end = %+v %v, want no results and the same total", p, err)
	}
}

func TestSearchReturnsOnlyTheNewestMatchingMemory(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	searchedRun(t, s, "a", nil, "first", "first", "solar panels: none yet")
	searchedRun(t, s, "a", nil, "second", "second", "solar panels: two installed")
	p, err := s.Search(ctx, store.SearchQuery{Q: "solar", Kinds: []string{"memory"}})
	if err != nil || len(p.Results) != 1 || !strings.Contains(p.Results[0].Snippet, "two installed") {
		t.Fatalf("memory search = %+v %v, want only the newest version", p, err)
	}
}

func TestSearchBackfillsRowsWrittenBeforeIt(t *testing.T) {
	s := testdb.Empty(t)
	if err := s.MigrateTo(ctx, searchMigration-1); err != nil {
		t.Fatal(err)
	}
	seed(t, s, "a")
	if _, err := s.SendToAgent(ctx, "a", "the heat pump is loud", "kanishk", nil); err != nil {
		t.Fatal(err)
	}
	searchedRun(t, s, "a", []string{`{"type":"assistant","message":{"content":[{"type":"text","text":"Checking the heat pump logs."}]}}`},
		"Heat pump fine", "The heat pump ran normally.", "heat pump: quiet mode on")
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := s.Search(ctx, store.SearchQuery{Q: `"heat pump"`})
	if err != nil {
		t.Fatal(err)
	}
	got := kindsOf(p)
	for _, k := range []string{"message", "report", "narration", "memory"} {
		if got[k] != 1 {
			t.Errorf("backfilled %s: %d, want 1 (all: %v)", k, got[k], got)
		}
	}
}

func TestSearchIndexingNeverFailsAWrite(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := manual(t, s, "a")
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil {
		t.Fatal(c, err)
	}
	for i, line := range []string{`"just a string"`, `{"message":{"content":"not an array"}}`, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":null}]}}`} {
		if err := s.AppendEvent(ctx, id, c.Attempt, i+1, "assistant", []byte(line)); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
	}
	evs, err := s.ListEvents(ctx, id, 0, 0, 10)
	if err != nil || len(evs) != 3 {
		t.Fatalf("events = %d %v", len(evs), err)
	}
}

func TestSearchShowsAMemoryEntryAsItIsNow(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	who := store.Provenance{Source: "person", Author: "kanishk"}
	prof, err := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile},
		EntryFields: store.EntryFields{Type: "user", Title: "Lives in Brooklyn", Body: "Rents in Brooklyn; the landlord is slow."}}, who)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: "agent", Owner: "a"},
		EntryFields: store.EntryFields{Type: "feedback", Title: "Landlord emails", Body: "Summarise landlord emails in one line."}}, who); err != nil {
		t.Fatal(err)
	}
	body := "Rents in Queens now."
	if _, err := s.UpdateEntry(ctx, prof.ID, prof.Version, store.EntryEdit{Body: &body}, who); err != nil {
		t.Fatal(err)
	}

	p, err := s.Search(ctx, store.SearchQuery{Q: "landlord", Kinds: []string{"memory_entry"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Results) != 1 || *p.Results[0].Title != "Landlord emails" || *p.Results[0].Agent != "a" || *p.Results[0].Scope != "agent" {
		t.Fatalf("landlord = %+v, want only the agent's entry (the profile's current version no longer says it)", p.Results)
	}
	p, err = s.Search(ctx, store.SearchQuery{Q: "brooklyn"})
	if err != nil || len(p.Results) != 1 || p.Results[0].Agent != nil || *p.Results[0].EntryVersion != 2 {
		t.Fatalf("brooklyn = %+v %v, want the profile entry's current version, by its title", p, err)
	}
	if p, err := s.Search(ctx, store.SearchQuery{Q: "brooklyn", Agent: "a"}); err != nil || p.Total != 0 {
		t.Fatalf("an agent filter matched a profile entry: %+v %v", p, err)
	}
	if _, err := s.RetireEntry(ctx, prof.ID, 2, nil, who); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Search(ctx, store.SearchQuery{Q: "queens"}); err != nil || p.Total != 0 {
		t.Fatalf("a retired entry matched: %+v %v", p, err)
	}
}
