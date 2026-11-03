package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestRestoreCheck(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{"a": {SHA: "s1", JSON: []byte(`{"name":"a"}`), Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: "s1", Trigger: "manual", Prompt: "hi"}); err != nil {
		t.Fatal(err)
	}
	ro, err := store.OpenReadOnly(ctx, s.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.Pool.Exec(ctx, `DELETE FROM runs`); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("a write on the read-only store: %v", err)
	}

	counts, err := parseCounts(strings.NewReader("agents\t1\nruns\t1\n\nconversations\t1\n"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := restoreCheck(ctx, ro, counts, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{"3 table(s) hold at least", "read 1 agent(s), 1 recent run(s)", "restore-check: ok"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	counts["runs"], counts["gone"] = 2, 1
	err = restoreCheck(ctx, ro, counts, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "gone is missing") || !strings.Contains(err.Error(), "runs has 1 rows, the dump 2") {
		t.Fatalf("short copy: %v", err)
	}
	// The ordinary store is not read only, so it is refused.
	if err := restoreCheck(ctx, s, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "not read only") {
		t.Fatalf("read-write store: %v", err)
	}
	if _, err := parseCounts(strings.NewReader("runs 1\n")); err == nil {
		t.Fatal("a line without a tab parsed")
	}
}

func TestRestoreCheckSchemaBehind(t *testing.T) {
	ctx := context.Background()
	s := testdb.Empty(t)
	if err := s.MigrateTo(ctx, 15); err != nil {
		t.Fatal(err)
	}
	ro, err := store.OpenReadOnly(ctx, s.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := restoreCheck(ctx, ro, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "behind this binary") {
		t.Fatalf("behind: %v", err)
	}
}
