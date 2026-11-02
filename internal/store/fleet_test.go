package store_test

import (
	"errors"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestClaimsFollowCapabilities(t *testing.T) {
	s := testdb.New(t)
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{
		"anywhere": {SHA: "sha-anywhere", JSON: []byte(`{"name":"anywhere"}`), Enabled: true},
		"finance":  {SHA: "sha-finance", JSON: []byte(`{"name":"finance","needs":["docker-network:hetzner","sure"]}`), Enabled: true},
		"browse":   {SHA: "sha-browse", JSON: []byte(`{"name":"browse","needs":["browser"]}`), Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	finance := manual(t, s, "finance")
	browse := manual(t, s, "browse")
	anywhere := manual(t, s, "anywhere")

	claim := func(c store.Claim) string {
		t.Helper()
		got, err := s.ClaimRunFor(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			return ""
		}
		return got.ID
	}
	// A worker with no capabilities passes over the older runs it cannot do and takes the one that needs nothing.
	if got := claim(store.Claim{Worker: "bare"}); got != anywhere {
		t.Fatalf("bare worker claimed %q, want the needs-free run", got)
	}
	if got := claim(store.Claim{Worker: "bare"}); got != "" {
		t.Fatalf("bare worker claimed %q, want nothing", got)
	}
	// A subset of the needs is not enough.
	if got := claim(store.Claim{Worker: "half", Capabilities: []string{"sure", "browser"}}); got != browse {
		t.Fatalf("half claimed %q, want browse", got)
	}
	if got := claim(store.Claim{Worker: "half", Capabilities: []string{"sure"}}); got != "" {
		t.Fatalf("a worker lacking docker-network:hetzner claimed %q", got)
	}
	if got := claim(store.Claim{Worker: "hetzner", Capabilities: []string{"docker-network:hetzner", "sure", "browser"}}); got != finance {
		t.Fatalf("hetzner claimed %q, want finance", got)
	}

	// The current spec decides: an agent that gains a need no longer runs on a host without it.
	if err := s.FinishRun(ctx, anywhere, store.RunResult{Attempt: 1, Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	manual(t, s, "anywhere")
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{
		"anywhere": {SHA: "sha-anywhere-2", JSON: []byte(`{"name":"anywhere","needs":["sandbox"]}`), Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	if got := claim(store.Claim{Worker: "bare"}); got != "" {
		t.Fatalf("claimed %q under a spec that now needs sandbox", got)
	}
	if got := claim(store.Claim{Worker: "control", Any: true}); got == "" {
		t.Fatal("a host advertising * claims everything")
	}
}

func TestWorkersNeverClaimBuilds(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r := approved(t, s)
	runID, err := s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: "build"})
	if err != nil {
		t.Fatal(err)
	}
	if c, err := s.ClaimRunFor(ctx, store.Claim{Worker: "oci", Any: true}); err != nil || c != nil {
		t.Fatalf("a worker claimed a build: %v %v", c, err)
	}
	c, err := s.ClaimRunFor(ctx, store.Claim{Worker: "hetzner", Any: true, Builds: true})
	if err != nil || c == nil || c.ID != runID {
		t.Fatalf("control did not claim the build: %v %v", c, err)
	}
}

func TestStoredConfigAndSessions(t *testing.T) {
	s := testdb.New(t)
	if _, _, err := s.StoredConfig(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("before control syncs: %v", err)
	}
	seed(t, s, "a", "b")
	for _, sha := range []string{"d1", "d2"} {
		if err := s.SyncDeployment(ctx, sha, []byte(`{"timezone":"`+sha+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
	seed(t, s, "a")
	dep, agents, err := s.StoredConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if string(dep) != `{"timezone": "d2"}` || len(agents) != 1 || agents[0].Name != "a" || agents[0].SHA != "sha-a" {
		t.Fatalf("stored config = %s %+v", dep, agents)
	}
	var versions int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM deployment_specs`).Scan(&versions)
	if versions != 2 {
		t.Fatalf("deployment versions kept = %d, want 2", versions)
	}

	id := manual(t, s, "a")
	if _, err := s.GetSessionTranscript(ctx, "s-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session: %v", err)
	}
	for _, body := range []string{"first", "second"} {
		if err := s.PutSessionTranscript(ctx, store.SessionTranscript{SessionID: "s-1", Agent: "a", RunID: id, Attempt: 1,
			Archive: []byte(body), Bytes: 5}); err != nil {
			t.Fatal(err)
		}
	}
	if b, err := s.GetSessionTranscript(ctx, "s-1"); err != nil || string(b) != "first" {
		t.Fatalf("a session id is stored once: %q %v", b, err)
	}
}

func TestOutboxIsClaimedOnceAndKept(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := manual(t, s, "a")
	if err := s.Enqueue(ctx, "oci", store.OutboxItem{Agent: "a", RunID: &id, Purpose: "report", Message: []byte(`{"Title":"t"}`)}); err != nil {
		t.Fatal(err)
	}
	it, err := s.ClaimOutbox(ctx)
	if err != nil || it == nil || *it.RunID != id || it.Purpose != "report" {
		t.Fatalf("claim: %+v %v", it, err)
	}
	if again, err := s.ClaimOutbox(ctx); err != nil || again != nil {
		t.Fatalf("claimed twice: %+v %v", again, err)
	}
	if err := s.OutboxSent(ctx, it.ID); err != nil {
		t.Fatal(err)
	}
	var kept int
	_ = s.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE sent_at IS NOT NULL`).Scan(&kept)
	if kept != 1 {
		t.Fatal("a sent notification is kept")
	}
}
