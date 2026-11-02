package engine_test

import (
	"strings"
	"testing"

	orbitharness "github.com/kensac/orbit/internal/harness"
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
