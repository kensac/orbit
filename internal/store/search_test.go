package store_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// searchMigration is the migration that creates search_docs.
const searchMigration = 20261102032149

// searchedRun runs one claimed run of agent to success with a transcript of
// events, a report and a memory document. It reads no context, so it also
// runs on the schema before search (TestSearchBackfillsRowsWrittenBeforeIt).
func searchedRun(t *testing.T, s *store.Store, agent string, events []string, summary, report, memory string) string {
	t.Helper()
	id := manual(t, s, agent)
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil || c.ID != id {
		t.Fatalf("claim: %v %v", c, err)
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

// codeEvents is a transcript line naming identifiers in prose and three tool
// calls naming a repository and files.
const codeEvents = `{"type":"assistant","message":{"content":[` +
	`{"type":"text","text":"The bug is in parseConfig, which reads internal/store/search.go before getHTTPResponse."},` +
	`{"type":"tool_use","name":"Read","input":{"file_path":"/data/runs/x/inputs/repos/orbit/internal/store/search.go"}},` +
	`{"type":"tool_use","name":"mcp__orbit__git_show","input":{"repo":"kensac/orbit","rev":"main","path":"cmd/orbit/main.go"}},` +
	`{"type":"tool_use","name":"Grep","input":{"pattern":"parseConfig","path":"internal/spec"}}]}}`

func searchOne(t *testing.T, s *store.Store, q store.SearchQuery) *store.SearchPage {
	t.Helper()
	p, err := s.Search(ctx, q)
	if err != nil {
		t.Fatalf("search %q: %v", q.Q, err)
	}
	return p
}

func TestSearchFindsIdentifiersHoweverTheyAreWritten(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "dev")
	if _, err := s.SendToAgent(ctx, "dev", "why does parse_config panic on an empty file?", "kanishk", nil); err != nil {
		t.Fatal(err)
	}
	searchedRun(t, s, "dev", []string{codeEvents}, "done", "Fixed it.", "")

	for _, q := range []string{"parseConfig", "parse_config", "parse-config", `"parse config"`, "ParseConfig"} {
		got := kindsOf(searchOne(t, s, store.SearchQuery{Q: q}))
		if got["message"] != 1 || got["narration"] != 1 || got["tool_call"] != 1 {
			t.Errorf("%s found %v, want the message, the narration and the Grep call", q, got)
		}
	}
	if got := kindsOf(searchOne(t, s, store.SearchQuery{Q: "search.go"})); got["narration"] != 1 || got["tool_call"] != 1 {
		t.Errorf("search.go found %v, want the narration and the Read", got)
	}
	if got := kindsOf(searchOne(t, s, store.SearchQuery{Q: "HTTP response"})); got["narration"] != 1 {
		t.Errorf("HTTP response found %v, want the narration naming getHTTPResponse", got)
	}
	// A piece out of order is not the identifier.
	if p := searchOne(t, s, store.SearchQuery{Q: "config_parse"}); p.Total != 0 {
		t.Errorf("config_parse matched %d", p.Total)
	}
	// A stop word alone still matches nothing, though the code index keeps it.
	if p := searchOne(t, s, store.SearchQuery{Q: "the"}); p.Total != 0 {
		t.Errorf("the matched %d", p.Total)
	}
	// Exclusion holds whichever index matched: the english side stems
	// "panics" to the message's "panic".
	if got := kindsOf(searchOne(t, s, store.SearchQuery{Q: "parse_config -panics"})); got["message"] != 0 || got["narration"] != 1 {
		t.Errorf("parse_config -panics found %v, want the message excluded", got)
	}
	n := searchOne(t, s, store.SearchQuery{Q: "parseConfig", Kinds: []string{"narration"}})
	if len(n.Results) != 1 || n.Results[0].Parts == nil {
		t.Fatalf("narration = %+v", n)
	}
}

func TestSearchOperators(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "dev", "other")
	searchedRun(t, s, "dev", []string{codeEvents}, "done", "Fixed it.", "")
	searchedRun(t, s, "other", []string{codeEvents}, "done", "Fixed it too.", "")

	cases := []struct {
		q     string
		total int
		tool  string
	}{
		{"search kind:tool_call agent:dev", 1, "Read"},
		{"search kind:tool_call,narration agent:dev", 2, ""},
		{"main repo:kensac/orbit agent:dev", 1, "mcp__orbit__git_show"},
		{"main repo:orbit agent:dev", 1, "mcp__orbit__git_show"},
		{"search repo:orbit agent:dev", 1, "Read"},
		{"main repo:someone/orbit agent:dev", 0, ""},
		{"search repo:kensac/orbit agent:dev", 1, "Read"},
		{"main path:cmd/orbit agent:dev", 1, "mcp__orbit__git_show"},
		{`parseConfig path:"internal/SPEC" agent:dev`, 1, "Grep"},
		{"parseConfig path:nowhere", 0, ""},
		{"search kind:tool_call", 2, "Read"},
	}
	for _, c := range cases {
		p := searchOne(t, s, store.SearchQuery{Q: c.q})
		if p.Total != c.total {
			t.Errorf("%s: total %d, want %d (%+v)", c.q, p.Total, c.total, p.Results)
			continue
		}
		if c.tool != "" && (p.Results[0].Tool == nil || *p.Results[0].Tool != c.tool) {
			t.Errorf("%s: first result %+v, want %s", c.q, p.Results[0], c.tool)
		}
	}
	read := searchOne(t, s, store.SearchQuery{Q: "search kind:tool_call agent:dev"}).Results[0]
	if read.Repo == nil || *read.Repo != "orbit" || read.Path == nil || !strings.HasSuffix(*read.Path, "internal/store/search.go") {
		t.Errorf("the Read's repo and path = %v %v", read.Repo, read.Path)
	}
	// The kind parameter and kind: both apply; agent: may not widen the
	// agent parameter.
	if p := searchOne(t, s, store.SearchQuery{Q: "search kind:tool_call,narration", Kinds: []string{"narration"}, Agent: "dev"}); p.Total != 1 || p.Results[0].Kind != "narration" {
		t.Errorf("kind and kind: = %+v", p)
	}
	for _, q := range []store.SearchQuery{
		{Q: "search agent:other", Agent: "dev"},
		{Q: "search kind:report", Kinds: []string{"narration"}},
		{Q: "search kind:bogus"},
		{Q: "agent:dev kind:report"},
		{Q: "search repo:"},
		{Q: "search path:a path:b"},
	} {
		if _, err := s.Search(ctx, q); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%+v: %v, want invalid", q, err)
		}
	}
	// A key that is not an operator is text: the narration names the URL.
	if p := searchOne(t, s, store.SearchQuery{Q: "bug https://example.com", Agent: "dev"}); p.Total != 0 {
		t.Errorf("an unknown key filtered: %+v", p)
	}
}

func TestSearchRepairsATypoOnlyWhenNothingMatches(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "finance")
	searchedRun(t, s, "finance", []string{electricityEvents}, "Electricity rose 12%", "The bill rose.", "")

	p := searchOne(t, s, store.SearchQuery{Q: "electrcity invoices"})
	if p.Total == 0 || p.Repaired == nil || p.Repaired.Q != "electricity invoices" ||
		len(p.Repaired.Words) != 1 || p.Repaired.Words[0] != (store.SearchRepairWord{From: "electrcity", To: "electricity"}) {
		t.Fatalf("electrcity = %+v (repaired %+v), want the repaired query's results", p, p.Repaired)
	}
	// Pieces of an identifier are repaired in place, keeping their case,
	// and the operators carry over.
	p = searchOne(t, s, store.SearchQuery{Q: "readElectrcity kind:narration"})
	if p.Repaired != nil {
		t.Fatalf("readElectrcity repaired to %+v though the phrase still matches nothing", p.Repaired)
	}
	p = searchOne(t, s, store.SearchQuery{Q: "Electrcity_invoices kind:narration"})
	if p.Repaired == nil || p.Repaired.Q != "Electricity_invoices kind:narration" || p.Total != 1 {
		t.Fatalf("Electrcity_invoices = %+v %+v", p, p.Repaired)
	}
	// A query that matches is never repaired, nor an excluded word.
	if p := searchOne(t, s, store.SearchQuery{Q: "electricity"}); p.Repaired != nil {
		t.Errorf("a matching query was repaired: %+v", p.Repaired)
	}
	if p := searchOne(t, s, store.SearchQuery{Q: "electricity -invoics"}); p.Repaired != nil || p.Total == 0 {
		t.Errorf("an excluded word was repaired: %+v %+v", p, p.Repaired)
	}
	if p := searchOne(t, s, store.SearchQuery{Q: "zyxwvut"}); p.Repaired != nil || p.Total != 0 {
		t.Errorf("a word with nothing close = %+v", p)
	}
}

func TestSearchFindsASkillAsItIsNow(t *testing.T) {
	s := testdb.New(t)
	who := store.SkillProvenance{Source: store.SkillByPerson, Author: "kanishk"}
	md := "---\nname: month-close\ndescription: Use to close a month of reconcileLedger books.\n---\n\nRun the ledger first.\n"
	if _, _, err := s.PutSkill(ctx, store.SkillWrite{Name: "month-close", Content: md, Create: true}, who); err != nil {
		t.Fatal(err)
	}
	p := searchOne(t, s, store.SearchQuery{Q: "reconcile ledger"})
	if p.Total != 1 || p.Results[0].Kind != "skill" || *p.Results[0].Skill != "month-close" || *p.Results[0].SkillVersion != 1 || p.Results[0].Agent != nil {
		t.Fatalf("skill search = %+v", p.Results)
	}
	if p := searchOne(t, s, store.SearchQuery{Q: "month close kind:skill"}); p.Total != 1 {
		t.Fatalf("by its name = %+v", p)
	}
	md2 := strings.Replace(md, "Run the ledger first.", "Start with the bank feed.", 1)
	if _, _, err := s.PutSkill(ctx, store.SkillWrite{Name: "month-close", Content: md2, Expect: 1}, who); err != nil {
		t.Fatal(err)
	}
	if p := searchOne(t, s, store.SearchQuery{Q: `"ledger first"`}); p.Total != 0 {
		t.Fatalf("an old version matched: %+v", p.Results)
	}
	if p := searchOne(t, s, store.SearchQuery{Q: "bank feed"}); p.Total != 1 || *p.Results[0].SkillVersion != 2 {
		t.Fatalf("current version = %+v", p.Results)
	}
	if _, err := s.RetireSkill(ctx, "month-close", 2, who); err != nil {
		t.Fatal(err)
	}
	if p := searchOne(t, s, store.SearchQuery{Q: "bank feed"}); p.Total != 0 {
		t.Fatalf("a retired skill matched: %+v", p.Results)
	}
}

func TestCodePiecesMatchTheDatabase(t *testing.T) {
	s := testdb.New(t)
	for _, w := range []string{"parseConfig", "HTTPServer", "getHTTPResponse", "internal/store/search.go", "store.Search",
		"parse_config", "utf8String", "ABCDeFGh", "v2.1.316", "x-y", "$HOME", "naïveCafé", "a+b=c|d~e`f", "ID", "aB1C", "--x--"} {
		var db string
		if err := s.Pool.QueryRow(ctx, `SELECT orbit_search_code_text($1)`, w).Scan(&db); err != nil {
			t.Fatal(err)
		}
		if got, want := strings.Join(store.CodePieces(w), " "), strings.Join(strings.Fields(db), " "); got != want {
			t.Errorf("%s: Go %q, database %q", w, got, want)
		}
	}
}
