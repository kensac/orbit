package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	orbitharness "github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

func TestRunsSearchTheirOwnAgentsHistoryOverTheSocket(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harness(t, fr)
	e.Harness = "/usr/local/bin/orbit"
	if err := e.Execute(ctx, claim(t, e, s)); err != nil {
		t.Fatal(err)
	}
	sock := fr.got.Harness.Search
	if sock == "" || len(sock) > 104 {
		t.Fatalf("search socket = %q, want one short enough for any unix socket", sock)
	}
	if !strings.Contains(fr.got.System, orbitharness.ToolSearch) {
		t.Fatal("the system prompt does not offer the search tool")
	}

	res, err := orbitharness.Search(sock, orbitharness.SearchRequest{Q: "baseline", Kind: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	results := res["results"].([]any)
	if res["total"] != 1.0 || !strings.Contains(results[0].(map[string]any)["snippet"].(string), "«baseline»") {
		t.Fatalf("search = %v, want the memory with the match marked", res)
	}
	if _, err := orbitharness.Search(sock, orbitharness.SearchRequest{Q: "x", Kind: "nope"}); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("an unknown kind = %v, want it named", err)
	}

	// The second run reuses the agent's socket: a live process's tool
	// server, started for the first run, keeps reaching it.
	if err := e.Execute(ctx, claim(t, e, s)); err != nil {
		t.Fatal(err)
	}
	if fr.got.Harness.Search != sock {
		t.Fatalf("second run's socket = %q, want %q", fr.got.Harness.Search, sock)
	}
}

// A run searches the memory entries it reads (its own, the profile's, its topics') and no other topic's; a chat run has the same search among orbit's tools.
func TestSearchCoversTheEntriesARunReads(t *testing.T) {
	var found map[string]any
	fr := &fakeRunner{output: `{"reply": "Done."}`}
	e, s, _ := harnessWith(t, fr, "topics: [money]\nchat: true\n")
	fr.seen = func(req runner.Request) {
		if req.Chat == nil {
			t.Error("not a chat run")
			return
		}
		text, isErr := req.Chat.Call(ctx, "search", json.RawMessage(`{"q": "zebra", "kind": "memory_entry"}`))
		if isErr {
			t.Errorf("search: %s", text)
			return
		}
		_ = json.Unmarshal([]byte(text), &found)
		if text, isErr := req.Chat.Call(ctx, "search", json.RawMessage(`{"q": "zebra", "agent": "other"}`)); !isErr || !strings.Contains(text, "agent") {
			t.Errorf("a field the schema does not have = %s", text)
		}
	}
	p := store.Provenance{Source: store.SourcePerson, Author: "kanishk"}
	for _, n := range []store.NewEntry{
		{EntryRef: store.EntryRef{Scope: store.ScopeProfile}},
		{EntryRef: store.EntryRef{Scope: store.ScopeTopic, Owner: "money"}},
		{EntryRef: store.EntryRef{Scope: store.ScopeTopic, Owner: "elsewhere"}},
		{EntryRef: store.EntryRef{Scope: store.ScopeAgent, Owner: "finance"}},
		{EntryRef: store.EntryRef{Scope: store.ScopeAgent, Owner: "other"}},
	} {
		n.EntryFields = store.EntryFields{Type: "project", Title: "Zebra " + n.Scope + " " + n.Owner, Body: "A zebra fact."}
		if _, err := s.CreateEntry(ctx, n, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.Execute(ctx, claim(t, e, s)); err != nil {
		t.Fatal(err)
	}
	if found["total"] != 3.0 {
		t.Fatalf("search = %v, want the profile's, money's and the agent's own entries", found)
	}
	if !strings.Contains(fr.got.System, "mcp__orbit__search") {
		t.Fatal("the chat's system prompt does not offer search")
	}
}
