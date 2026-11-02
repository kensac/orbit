package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func person() store.Provenance {
	return store.Provenance{Source: store.SourcePerson, Author: "kanishk"}
}

func fields(typ, title, body string) store.EntryFields {
	return store.EntryFields{Type: typ, Title: title, Body: body}
}

func TestEntryVersionsAreKept(t *testing.T) {
	s := testdb.New(t)
	e, err := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile},
		EntryFields: fields("feedback", "No AI attribution", "Never name an AI in a commit.")}, person())
	if err != nil {
		t.Fatal(err)
	}
	if e.Key != "no-ai-attribution" || e.Version != 1 || e.Status != "active" || *e.Author != "kanishk" {
		t.Fatalf("created = %+v", e)
	}
	why := "He asked twice."
	e2, err := s.UpdateEntry(ctx, e.ID, 1, store.EntryEdit{Why: &why}, person())
	if err != nil {
		t.Fatal(err)
	}
	if e2.Version != 2 || e2.Why != why || e2.Body != e.Body {
		t.Fatalf("updated = %+v", e2)
	}
	// An edit made against version 1 now conflicts instead of overwriting.
	body := "stale"
	if _, err := s.UpdateEntry(ctx, e.ID, 1, store.EntryEdit{Body: &body}, person()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale edit = %v, want ErrConflict", err)
	}
	// An edit that changes nothing writes no version.
	if same, err := s.UpdateEntry(ctx, e.ID, 2, store.EntryEdit{Why: &why}, person()); err != nil || same.Version != 2 {
		t.Fatalf("no-op edit = %+v, %v", same, err)
	}
	cur, versions, err := s.GetEntry(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Version != 2 || len(versions) != 2 || versions[0].Version != 2 || versions[1].Why != "" {
		t.Fatalf("history = %+v / %+v", cur, versions)
	}
	if _, _, err := s.GetEntry(ctx, "not-a-uuid"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bad id = %v", err)
	}
}

func TestEntryValidation(t *testing.T) {
	s := testdb.New(t)
	bad := []store.NewEntry{
		{EntryRef: store.EntryRef{Scope: "world"}, EntryFields: fields("user", "t", "b")},
		{EntryRef: store.EntryRef{Scope: store.ScopeTopic}, EntryFields: fields("user", "t", "b")},
		{EntryRef: store.EntryRef{Scope: store.ScopeProfile, Owner: "x"}, EntryFields: fields("user", "t", "b")},
		{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, EntryFields: fields("opinion", "t", "b")},
		{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, EntryFields: fields("user", "", "b")},
		{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, EntryFields: fields("user", "t", "")},
		{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, Key: "Bad Key", EntryFields: fields("user", "t", "b")},
	}
	for i, n := range bad {
		if _, err := s.CreateEntry(ctx, n, person()); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%d: err = %v, want ErrInvalid", i, err)
		}
	}
}

func TestEntryKeysAreUniquePerScope(t *testing.T) {
	s := testdb.New(t)
	n := store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeTopic, Owner: "finance"}, EntryFields: fields("project", "Rent", "Rent is $2,100.")}
	a, _ := s.CreateEntry(ctx, n, person())
	b, err := s.CreateEntry(ctx, n, person())
	if err != nil || a.Key != "rent" || b.Key != "rent-2" {
		t.Fatalf("derived keys = %q, %q (%v)", a.Key, b.Key, err)
	}
	n.Key = "rent"
	if _, err := s.CreateEntry(ctx, n, person()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("explicit duplicate key = %v, want ErrConflict", err)
	}
	// The same key in another topic is another entry.
	n.Owner = "homelab"
	if _, err := s.CreateEntry(ctx, n, person()); err != nil {
		t.Fatal(err)
	}
}

func TestRetireAndSupersede(t *testing.T) {
	s := testdb.New(t)
	old, _ := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, EntryFields: fields("user", "Lives in Boston", "Boston.")}, person())
	repl, _ := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, EntryFields: fields("user", "Lives in New York", "New York.")}, person())
	if _, err := s.RetireEntry(ctx, old.ID, 0, &old.ID, person()); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("self-supersede = %v", err)
	}
	r, err := s.RetireEntry(ctx, old.ID, 1, &repl.ID, person())
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "retired" || r.SupersededBy == nil || *r.SupersededBy != repl.ID || r.Version != 2 {
		t.Fatalf("retired = %+v", r)
	}
	// A retired entry cannot supersede another.
	third, _ := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, EntryFields: fields("user", "x", "y")}, person())
	if _, err := s.RetireEntry(ctx, third.ID, 0, &old.ID, person()); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("superseded by a retired entry = %v", err)
	}
	active, _ := s.ListEntries(ctx, store.EntryFilter{Scope: store.ScopeProfile})
	if len(active) != 2 {
		t.Fatalf("active = %d entries, want 2", len(active))
	}
	retired, _ := s.ListEntries(ctx, store.EntryFilter{Status: "retired"})
	if len(retired) != 1 || retired[0].ID != old.ID {
		t.Fatalf("retired = %+v", retired)
	}
	// Restoring is a version too, and clears superseded_by.
	act := "active"
	back, err := s.UpdateEntry(ctx, old.ID, 2, store.EntryEdit{Status: &act}, person())
	if err != nil || back.Status != "active" || back.SupersededBy != nil || back.Version != 3 {
		t.Fatalf("restored = %+v, %v", back, err)
	}
}

func TestEntriesForAnAgent(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "finance", "homelab")
	mk := func(scope, owner, typ, title string) {
		t.Helper()
		if _, err := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: scope, Owner: owner}, EntryFields: fields(typ, title, title)}, person()); err != nil {
			t.Fatal(err)
		}
	}
	mk(store.ScopeProfile, "", "user", "profile fact")
	mk(store.ScopeTopic, "finance", "feedback", "finance pref")
	mk(store.ScopeTopic, "homelab", "project", "homelab fact")
	mk(store.ScopeAgent, "finance", "project", "finance agent fact")
	mk(store.ScopeAgent, "homelab", "project", "homelab agent fact")
	got, err := s.EntriesFor(ctx, "finance", []string{"finance"})
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, e := range got {
		titles = append(titles, e.Title)
	}
	want := []string{"profile fact", "finance pref", "finance agent fact"}
	if len(titles) != len(want) {
		t.Fatalf("titles = %v, want %v", titles, want)
	}
	for i := range want {
		if titles[i] != want[i] {
			t.Fatalf("titles = %v, want %v", titles, want)
		}
	}
	q, _ := s.ListEntries(ctx, store.EntryFilter{Query: "HOMELAB"})
	if len(q) != 2 {
		t.Fatalf("query = %d, want 2", len(q))
	}
}

func TestImportIsIdempotentAndDryRunWritesNothing(t *testing.T) {
	s := testdb.New(t)
	imp := store.Provenance{Source: store.SourceImport, Note: "test"}
	entries := []store.NewEntry{
		{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, Key: "who", EntryFields: fields("user", "Who", "A person.")},
		{EntryRef: store.EntryRef{Scope: store.ScopeTopic, Owner: "homelab"}, Key: "kener", EntryFields: fields("feedback", "Kener stays", "Keep it.")},
	}
	dry, err := s.ImportEntries(ctx, entries, imp, true)
	if err != nil || len(dry) != 2 || dry[0].Outcome != "created" || dry[0].ID != "" {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	if all, _ := s.ListEntries(ctx, store.EntryFilter{Status: "all"}); len(all) != 0 {
		t.Fatalf("dry run wrote %d entries", len(all))
	}
	first, err := s.ImportEntries(ctx, entries, imp, false)
	if err != nil || first[1].Outcome != "created" || first[1].ID == "" {
		t.Fatalf("import = %+v, %v", first, err)
	}
	again, _ := s.ImportEntries(ctx, entries, imp, false)
	if again[0].Outcome != "unchanged" || again[1].Outcome != "unchanged" {
		t.Fatalf("re-import = %+v", again)
	}
	entries[1].Body = "Keep it, always."
	changed, _ := s.ImportEntries(ctx, entries, imp, false)
	if changed[1].Outcome != "updated" || changed[1].Version != 2 {
		t.Fatalf("changed import = %+v", changed)
	}
	// A retired entry is brought back by an import that names it.
	if _, err := s.RetireEntry(ctx, changed[1].ID, 0, nil, person()); err != nil {
		t.Fatal(err)
	}
	back, _ := s.ImportEntries(ctx, entries, imp, false)
	if back[1].Outcome != "updated" {
		t.Fatalf("import of a retired entry = %+v", back)
	}
	e, versions, _ := s.GetEntry(ctx, changed[1].ID)
	if e.Status != "active" || len(versions) != 4 || e.Source != store.SourceImport {
		t.Fatalf("after import = %+v, %d versions", e, len(versions))
	}
	dup := append(entries, entries[0])
	if _, err := s.ImportEntries(ctx, dup, imp, false); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("duplicate keys = %v", err)
	}
}

func TestAttachments(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	if created, err := s.Attach(ctx, "a", store.AttachTopic, "finance", "kanishk"); err != nil || !created {
		t.Fatalf("attach = %v, %v", created, err)
	}
	if created, err := s.Attach(ctx, "a", store.AttachTopic, "finance", "kanishk"); err != nil || created {
		t.Fatalf("re-attach = %v, %v", created, err)
	}
	if _, err := s.Attach(ctx, "nobody", store.AttachTopic, "finance", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown agent = %v", err)
	}
	if _, err := s.Attach(ctx, "a", store.AttachTopic, "Bad", ""); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("bad name = %v", err)
	}
	if _, err := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeTopic, Owner: "homelab"}, EntryFields: fields("user", "t", "b")}, person()); err != nil {
		t.Fatal(err)
	}
	topics, err := s.ListTopics(ctx)
	if err != nil || len(topics) != 2 || topics[0].Name != "finance" || topics[0].Agents[0] != "a" || topics[1].Entries != 1 {
		t.Fatalf("topics = %+v, %v", topics, err)
	}
	if err := s.Detach(ctx, "a", store.AttachTopic, "finance", "kanishk"); err != nil {
		t.Fatal(err)
	}
	if err := s.Detach(ctx, "a", store.AttachTopic, "finance", "kanishk"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("detach twice = %v", err)
	}
	if names, _ := s.AttachedNames(ctx, "a", store.AttachTopic); len(names) != 0 {
		t.Fatalf("attached after detach = %v", names)
	}
}

// Pinning is a field like any other: versioned, kept by edits that do not name it, only on feedback, and dropped when an entry stops being feedback.
func TestPinnedEntries(t *testing.T) {
	s := testdb.New(t)
	f := fields("feedback", "No em dashes", "Never write an em dash.")
	f.Pinned = true
	e, err := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, EntryFields: f}, person())
	if err != nil || !e.Pinned {
		t.Fatalf("create pinned = %+v %v", e, err)
	}
	u := fields("user", "Lives by the sea", "x")
	u.Pinned = true
	if _, err := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, EntryFields: u}, person()); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a pinned user entry = %v, want ErrInvalid", err)
	}
	why := "He asked."
	e2, err := s.UpdateEntry(ctx, e.ID, 1, store.EntryEdit{Why: &why}, person())
	if err != nil || !e2.Pinned || e2.Version != 2 {
		t.Fatalf("an edit not naming pinned = %+v %v", e2, err)
	}
	off := false
	e3, err := s.UpdateEntry(ctx, e.ID, 2, store.EntryEdit{Pinned: &off}, person())
	if err != nil || e3.Pinned || e3.Version != 3 {
		t.Fatalf("unpin = %+v %v", e3, err)
	}
	if e4, err := s.RestoreEntry(ctx, e.ID, 2, 3, person()); err != nil || !e4.Pinned {
		t.Fatalf("restore of a pinned version = %+v %v", e4, err)
	}
	typ := "project"
	e5, err := s.UpdateEntry(ctx, e.ID, 4, store.EntryEdit{Type: &typ}, person())
	if err != nil || e5.Pinned {
		t.Fatalf("an entry that stops being feedback = %+v %v", e5, err)
	}
	on := true
	if _, err := s.UpdateEntry(ctx, e.ID, 5, store.EntryEdit{Pinned: &on}, person()); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("pinning a project entry = %v", err)
	}
	_, versions, _ := s.GetEntry(ctx, e.ID)
	var got []bool
	for _, v := range versions {
		got = append(got, v.Pinned)
	}
	if fmt.Sprint(got) != "[false true false true true]" {
		t.Fatalf("pinned by version, newest first = %v", got)
	}

	// An import leaves an existing entry's pin alone: a Claude Code memory file has none.
	g := fields("feedback", "Short replies", "Keep replies short.")
	g.Pinned = true
	pinned, _ := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, EntryFields: g}, person())
	in := []store.NewEntry{{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, Key: pinned.Key, EntryFields: fields("feedback", "Short replies", "Keep replies short, always.")}}
	if _, err := s.ImportEntries(ctx, in, store.Provenance{Source: store.SourceImport}, false); err != nil {
		t.Fatal(err)
	}
	if cur, _, _ := s.GetEntry(ctx, pinned.ID); !cur.Pinned || cur.Version != 2 {
		t.Fatalf("after import = %+v", cur)
	}
}
