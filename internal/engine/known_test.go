package engine_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/engine"
	orbitharness "github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/store"
)

func TestRunIsShownTheEntriesItReads(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, "topics: [money]\n")
	p := store.Provenance{Source: store.SourcePerson, Author: "kanishk"}
	mk := func(scope, owner, typ, title, how string) {
		t.Helper()
		_, err := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: scope, Owner: owner},
			EntryFields: store.EntryFields{Type: typ, Title: title, Body: title + " in full.", HowToApply: how}}, p)
		if err != nil {
			t.Fatal(err)
		}
	}
	mk(store.ScopeProfile, "", "feedback", "Plain words", "No jargon.")
	mk(store.ScopeTopic, "money", "project", "Rent is fixed", "")
	mk(store.ScopeTopic, "attached", "project", "Attached topic fact", "")
	mk(store.ScopeTopic, "elsewhere", "project", "Unrelated topic", "")
	mk(store.ScopeAgent, "finance", "reference", "Sure holds the ledger", "")
	if _, err := s.Attach(ctx, "finance", store.AttachTopic, "attached", "kanishk"); err != nil {
		t.Fatal(err)
	}

	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# What you know", "## Your person (profile, every agent)", "[feedback] Plain words (profile/plain-words). Apply: No jargon.",
		"## Topic: money", "(topic/money/rent-is-fixed)", "## Topic: attached", "## This agent", "(agent/sure-holds-the-ledger)"} {
		if !strings.Contains(fr.got.Prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, fr.got.Prompt)
		}
	}
	if strings.Contains(fr.got.Prompt, "Unrelated topic") {
		t.Fatal("a topic the agent does not read reached its prompt")
	}
	if !strings.Contains(fr.got.System, "Memory entries (What you know") {
		t.Fatal("the system prompt does not explain the entries")
	}
	var file string
	for _, in := range fr.got.Inputs {
		if strings.HasSuffix(in.Path, "_memory.md") {
			file = string(in.Content)
		}
	}
	if !strings.Contains(file, "ref: profile/plain-words") || !strings.Contains(file, "Sure holds the ledger in full.") {
		t.Fatalf("entries file = %q", file)
	}
	arts, _ := s.ListArtifacts(ctx, c.ID)
	found := false
	for _, a := range arts {
		found = found || a.Name == "inputs/_memory.md"
	}
	if !found {
		t.Fatal("the entries file is not stored as an artifact")
	}
}

func TestRunWithoutEntriesHasNoIndex(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harness(t, fr)
	if err := e.Execute(ctx, claim(t, e, s)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fr.got.Prompt, "What you know") || len(fr.got.Inputs) != 1 {
		t.Fatalf("an agent with no entries got an index or file: %d inputs", len(fr.got.Inputs))
	}
}

func TestKnownIndexStaysInItsBudget(t *testing.T) {
	var entries []store.Entry
	now := time.Now()
	for i := range 200 {
		entries = append(entries, store.Entry{Scope: store.ScopeProfile, Key: strings.Repeat("k", 10) + string(rune('a'+i%26)),
			EntryFields: store.EntryFields{Type: "project", Title: strings.Repeat("t", 150), Description: strings.Repeat("d", 300)},
			UpdatedAt:   now})
	}
	idx := engine.KnownIndex(entries, true)
	if len(idx) > 8<<10 {
		t.Fatalf("index is %d bytes", len(idx))
	}
	if !strings.Contains(idx, "more entries are not listed here (0 of them feedback)") || !strings.Contains(idx, "They are in inputs/_memory.md; find one by its words with mcp__orbit__search, kind memory_entry.") {
		t.Fatalf("the index does not say entries were left out, and how to reach them:\n%s", idx[len(idx)-400:])
	}
	if engine.KnownIndex(entries, true) != idx {
		t.Fatal("the index is not deterministic")
	}
	if strings.Contains(engine.KnownIndex(entries, false), "mcp__orbit__search") {
		t.Fatal("a run without the search tool is told to use it")
	}
}

// With many topics, the index keeps every feedback entry, then the most recently changed, whatever their scope or topic's name; what it lists stays grouped as before.
func TestKnownIndexKeepsFeedbackThenNewest(t *testing.T) {
	now := time.Now()
	var entries []store.Entry
	long := strings.Repeat("d", 380)
	for i := range 60 {
		// Topics a.. sort before the agent's own and the newest: under the old packing they filled the budget.
		entries = append(entries, store.Entry{Scope: store.ScopeTopic, Topic: "a" + string(rune('a'+i%20)), Key: "old-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			EntryFields: store.EntryFields{Type: "project", Title: "Old fact", Description: long}, UpdatedAt: now.Add(-time.Duration(100+i) * time.Hour)})
	}
	entries = append(entries,
		store.Entry{Scope: store.ScopeTopic, Topic: "zz", Key: "old-rule", EntryFields: store.EntryFields{Type: "feedback", Title: "An old rule", Description: long}, UpdatedAt: now.Add(-1000 * time.Hour)},
		store.Entry{Scope: store.ScopeAgent, Agent: "orbit", Key: "newest", EntryFields: store.EntryFields{Type: "project", Title: "The newest fact"}, UpdatedAt: now},
		store.Entry{Scope: store.ScopeProfile, Key: "second", EntryFields: store.EntryFields{Type: "user", Title: "The second newest"}, UpdatedAt: now.Add(-time.Hour)},
	)
	idx := engine.KnownIndex(entries, true)
	if len(idx) > 8<<10 {
		t.Fatalf("index is %d bytes", len(idx))
	}
	for _, want := range []string{"(topic/zz/old-rule)", "(agent/newest)", "(profile/second)", "## Topic: zz", "## This agent"} {
		if !strings.Contains(idx, want) {
			t.Fatalf("index is missing %q:\n%s", want, idx)
		}
	}
	// Grouped in the usual order: the profile, topics by name, the agent's own.
	if p, z, a := strings.Index(idx, "## Your person"), strings.Index(idx, "## Topic: zz"), strings.Index(idx, "## This agent"); p > z || z > a {
		t.Fatalf("groups out of order:\n%s", idx)
	}
	shown := strings.Count(idx, "- [project] Old fact")
	if shown == 0 || shown == 60 {
		t.Fatalf("%d old facts shown, want some but not all", shown)
	}
	if !strings.Contains(idx, fmt.Sprintf("(%d more entries are not listed here (0 of them feedback)", 60-shown)) {
		t.Fatalf("index note:\n%s", idx[len(idx)-300:])
	}
	// Every shown old fact is newer than every one left out.
	for i := range 60 {
		e := entries[i]
		ref := "(topic/" + e.Topic + "/" + e.Key + ")"
		if strings.Contains(idx, ref) != (i < shown) {
			t.Fatalf("old fact %d (%s) shown = %v, want the %d newest shown", i, ref, i >= shown, shown)
		}
	}
}

// An agent reading every topic ("*", as the built-in orbit agent does) is shown each topic's entries, may write to any of them, and finds them with its search; an agent naming one topic searches only that topic's and the profile's.
func TestAgentReadingEveryTopic(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, "topics: [\"*\"]\n")
	e.Harness = "/usr/local/bin/orbit"
	p := store.Provenance{Source: store.SourcePerson, Author: "kanishk"}
	for _, topic := range []string{"homelab", "money"} {
		if _, err := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeTopic, Owner: topic},
			EntryFields: store.EntryFields{Type: "feedback", Title: "Rule for " + topic, Body: "Zebra rule for " + topic + "."}}, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Execute(ctx, claim(t, e, s)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Topic: homelab", "## Topic: money"} {
		if !strings.Contains(fr.got.Prompt, want) {
			t.Fatalf("prompt is missing %q", want)
		}
	}
	if !strings.Contains(fr.got.System, "topic/<name> for a topic you read (homelab, money)") {
		t.Fatalf("system prompt does not offer both topics to write to:\n%s", fr.got.System)
	}
	res, err := orbitharness.Search(fr.got.Harness.Search, orbitharness.SearchRequest{Q: "zebra", Kind: "memory_entry"})
	if err != nil || res["total"] != 2.0 {
		t.Fatalf("search = %v %v, want both topics' entries", res, err)
	}
}
