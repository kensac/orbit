package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/engine"
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
	idx := engine.KnownIndex(entries)
	if len(idx) > 8<<10 {
		t.Fatalf("index is %d bytes", len(idx))
	}
	if !strings.Contains(idx, "more entries are only in inputs/_memory.md") {
		t.Fatal("the index does not say entries were left out")
	}
	if engine.KnownIndex(entries) != idx {
		t.Fatal("the index is not deterministic")
	}
}
