package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

var ctx = context.Background()

func seed(t *testing.T, s *store.Store, names ...string) {
	t.Helper()
	specs := map[string]store.AgentSpec{}
	for _, n := range names {
		specs[n] = store.AgentSpec{SHA: "sha-" + n, JSON: []byte(`{"name":"` + n + `"}`), Enabled: true}
	}
	if err := s.SyncAgents(ctx, specs); err != nil {
		t.Fatal(err)
	}
}

func manual(t *testing.T, s *store.Store, agent string) string {
	t.Helper()
	id, err := s.EnqueueRun(ctx, store.NewRun{Agent: agent, SpecSHA: "sha-" + agent, Trigger: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// oldAgent writes agent, its spec and its default conversation in SQL, for a test that migrates to an old version first: today's store code reads columns later migrations add. It returns the conversation.
func oldAgent(t *testing.T, s *store.Store, agent string) string {
	t.Helper()
	var conv string
	if err := s.Pool.QueryRow(ctx, `
		WITH spec AS (INSERT INTO agent_specs (sha, agent, spec) VALUES ('sha-' || $1, $1, '{}')),
		     a AS (INSERT INTO agents (name, spec_sha, enabled) VALUES ($1, 'sha-' || $1, true))
		INSERT INTO conversations (agent, title, is_default) VALUES ($1, 'General', true) RETURNING id`, agent).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	return conv
}

// oldRun writes a finished run of agent in conv with its one attempt in SQL, for the same tests as oldAgent. set is an UPDATE's SET list over runs, with args from $2.
func oldRun(t *testing.T, s *store.Store, agent, conv, set string, args ...any) string {
	t.Helper()
	var id string
	if err := s.Pool.QueryRow(ctx, `INSERT INTO runs (agent, spec_sha, trigger, conversation_id, attempt, status, worker, started_at, finished_at)
		VALUES ($1, 'sha-' || $1, 'manual', $2, 1, 'succeeded', 'w', now(), now()) RETURNING id`, agent, conv).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO run_attempts (run_id, attempt, worker) VALUES ($1, 1, 'w')`, id); err != nil {
		t.Fatal(err)
	}
	if set != "" {
		if _, err := s.Pool.Exec(ctx, `UPDATE runs SET `+set+` WHERE id = $1`, append([]any{id}, args...)...); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func TestSyncAgentsKeepsRemovedAgentsAbsent(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a", "b")
	seed(t, s, "a")
	agents, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, a := range agents {
		got[a.Name] = a.Present
	}
	if !got["a"] || got["b"] || len(got) != 2 {
		t.Fatalf("present = %v, want a present and b kept but absent", got)
	}
}

func TestScheduleFireEnqueuesOnce(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	at := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC)
	idx := 0
	r := store.NewRun{Agent: "a", SpecSHA: "sha-a", Trigger: "schedule", TriggerIndex: &idx, ScheduledFor: &at}
	first, err := s.EnqueueRun(ctx, r)
	if err != nil || first == "" {
		t.Fatalf("first enqueue: %q %v", first, err)
	}
	second, err := s.EnqueueRun(ctx, r)
	if err != nil || second != "" {
		t.Fatalf("second enqueue of the same fire = %q %v, want no row", second, err)
	}
	last, err := s.LastScheduledFire(ctx, "a", 0)
	if err != nil || last == nil || !last.Equal(at) {
		t.Fatalf("last fire = %v %v", last, err)
	}
}

func TestClaimRunsOneRunPerAgentAtATime(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a", "b")
	manual(t, s, "a")
	manual(t, s, "a")
	manual(t, s, "b")

	// Claim concurrently from many workers: exactly one run of each agent
	// may be running.
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.ClaimRun(ctx, "w")
			if err != nil {
				t.Error(err)
				return
			}
			if c != nil {
				mu.Lock()
				claimed[c.Agent]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claimed["a"] != 1 || claimed["b"] != 1 {
		t.Fatalf("claimed = %v, want one of each", claimed)
	}
}

func TestReapRequeuesThenFails(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := manual(t, s, "a")
	for attempt := 1; attempt <= store.MaxAttempts; attempt++ {
		c, err := s.ClaimRun(ctx, "w")
		if err != nil || c == nil || c.ID != id || c.Attempt != attempt {
			t.Fatalf("attempt %d: claim = %+v %v", attempt, c, err)
		}
		if _, err := s.Pool.Exec(ctx, `UPDATE runs SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
		failed, err := s.ReapStale(ctx, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		r, _ := s.GetRun(ctx, id)
		if attempt < store.MaxAttempts {
			if r.Status != "queued" || len(failed) != 0 {
				t.Fatalf("attempt %d: status %s, failed %v; want requeued", attempt, r.Status, failed)
			}
		} else if r.Status != "failed" || len(failed) != 1 {
			t.Fatalf("last attempt: status %s, failed %v; want failed", r.Status, failed)
		}
	}
}

func TestMessagesReachExactlyOneRunAndReturnOnFailure(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	if _, err := s.SendToAgent(ctx, "a", "hello", "me", nil); err != nil {
		t.Fatal(err)
	}
	id := manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	rc, err := s.StartRunContext(ctx, c.ID, "a")
	if err != nil || len(rc.Messages) != 1 {
		t.Fatalf("first run context = %+v %v", rc, err)
	}
	again, _ := s.StartRunContext(ctx, c.ID, "a")
	if len(again.Messages) != 0 {
		t.Fatal("a consumed message was delivered twice")
	}
	if err := s.FinishRun(ctx, id, store.RunResult{Attempt: 1, Status: "failed", Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	id2 := manual(t, s, "a")
	c2, _ := s.ClaimRun(ctx, "w")
	if c2 == nil || c2.ID != id2 {
		t.Fatalf("second claim = %+v", c2)
	}
	rc2, _ := s.StartRunContext(ctx, c2.ID, "a")
	if len(rc2.Messages) != 1 || rc2.Messages[0].Body != "hello" {
		t.Fatalf("after a failed run the message should return, got %+v", rc2.Messages)
	}
}

func TestOutcomeAndOpenQuestions(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	mem := "baseline: 2700"
	ids, err := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: 1, Status: "succeeded", Summary: "ok"}, store.Outcome{
		Memory:    &mem,
		Questions: []string{"Is the Enterprise charge a deductible?"},
		Actions: []store.ProposedAction{
			{Type: "notify", Params: json.RawMessage(`{}`), Rule: "auto"},
			{Type: "webhook:x", Params: json.RawMessage(`{}`), Rule: "approve"},
			{Type: "webhook:y", Params: json.RawMessage(`{}`), Rule: "deny", Error: "not enabled"},
		},
	})
	if err != nil || len(ids) != 3 {
		t.Fatalf("record outcome: %v %v", ids, err)
	}
	want := []string{"approved", "pending", "denied"}
	for i, aid := range ids {
		a, _ := s.GetAction(ctx, aid)
		if a.Status != want[i] {
			t.Errorf("action %d status = %s, want %s", i, a.Status, want[i])
		}
	}
	if m, _ := s.Memory(ctx, "a"); m != mem {
		t.Fatalf("memory = %q", m)
	}
	r, _ := s.GetRun(ctx, id)
	if r.Status != "succeeded" {
		t.Fatalf("run status %s", r.Status)
	}

	// The question is open until any message arrives after it.
	next := manual(t, s, "a")
	c2, _ := s.ClaimRun(ctx, "w")
	rc, _ := s.StartRunContext(ctx, c2.ID, "a")
	if len(rc.Questions) != 1 {
		t.Fatalf("open questions = %d, want 1", len(rc.Questions))
	}
	_ = s.FinishRun(ctx, next, store.RunResult{Attempt: 1, Status: "succeeded"})
	if _, err := s.SendToAgent(ctx, "a", "yes, a deductible", "me", nil); err != nil {
		t.Fatal(err)
	}
	manual(t, s, "a")
	c3, _ := s.ClaimRun(ctx, "w")
	rc3, _ := s.StartRunContext(ctx, c3.ID, "a")
	if len(rc3.Questions) != 0 {
		t.Fatalf("a later message should close the question, got %d open", len(rc3.Questions))
	}
}

func TestActionDecisionsAndSingleExecution(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	ids, _ := s.RecordOutcome(ctx, c.ID, "a", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{{Type: "notify", Params: json.RawMessage(`{}`), Rule: "approve"}},
	})
	if err := s.DecideAction(ctx, ids[0], true, "me"); err != nil {
		t.Fatal(err)
	}
	if err := s.DecideAction(ctx, ids[0], false, "me"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second decision = %v, want ErrConflict", err)
	}
	if err := s.DecideAction(ctx, "00000000-0000-0000-0000-000000000000", true, "me"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown action = %v, want ErrNotFound", err)
	}
	a, err := s.ClaimApprovedAction(ctx)
	if err != nil || a == nil || a.ID != ids[0] {
		t.Fatalf("claim approved = %+v %v", a, err)
	}
	if again, _ := s.ClaimApprovedAction(ctx); again != nil {
		t.Fatal("an executing action was claimed twice")
	}
	// Another live instance may be executing it right now: within the
	// bound it is left alone.
	if n, err := s.FailOrphanedActions(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("an action executing now was failed: %d %v", n, err)
	}
	// A crash mid-execution fails it rather than running it again.
	n, err := s.FailOrphanedActions(ctx, 0)
	if err != nil || n != 1 {
		t.Fatalf("orphaned = %d %v", n, err)
	}
}

func TestEventsAndArtifactsAreKeptPerAttempt(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	if err := s.AppendEvent(ctx, id, c.Attempt, 1, "x", []byte("not json")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutArtifact(ctx, id, c.Attempt, "in", "prompt.md", "text/markdown", []byte("p1")); err != nil {
		t.Fatal(err)
	}
	if err := s.HandBack(ctx, id, 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	c2, _ := s.ClaimRun(ctx, "w")
	if err := s.PutArtifact(ctx, id, c2.Attempt, "in", "prompt.md", "text/markdown", []byte("p2")); err != nil {
		t.Fatalf("second attempt's artifact: %v", err)
	}
	arts, _ := s.ListArtifacts(ctx, id)
	if len(arts) != 2 {
		t.Fatalf("artifacts = %d, want both attempts kept", len(arts))
	}
	ev, err := s.ListEvents(ctx, id, 1, 0, 0)
	if err != nil || len(ev) != 1 || string(ev[0].Payload) != `"not json"` {
		t.Fatalf("events = %+v %v", ev, err)
	}
	if latest, _ := s.ListEvents(ctx, id, 0, 0, 0); len(latest) != 0 {
		t.Fatalf("latest attempt has no events yet, got %d", len(latest))
	}
}

func TestConcurrentRetriesPostOnce(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	var wg sync.WaitGroup
	results := make([]store.PostResult, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: "hi", Wake: true, ClientID: "c"})
			if err != nil {
				t.Error(err)
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	for _, r := range results {
		if r != results[0] || r.RunID == "" {
			t.Fatalf("results differ: %+v", results)
		}
	}
	var msgs, runs int
	_ = s.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM messages), (SELECT count(*) FROM runs)`).Scan(&msgs, &runs)
	if msgs != 1 || runs != 1 {
		t.Fatalf("messages %d runs %d", msgs, runs)
	}
}

// defaultConv is the agent's default conversation.
func defaultConv(t *testing.T, s *store.Store, agent string) string {
	t.Helper()
	var id string
	if err := s.Pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE agent = $1 AND is_default`, agent).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAChatAgentRunsOnePerConversation(t *testing.T) {
	s := testdb.New(t)
	if err := s.SyncAgents(ctx, map[string]store.AgentSpec{
		"orbit": {SHA: "sha-orbit", JSON: []byte(`{"name":"orbit","chat":true}`), Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}
	var convs []string
	for i := 0; i < 2; i++ {
		r, _, err := s.CreateConversation(ctx, store.NewConversation{Agent: "orbit", SpecSHA: "sha-orbit", Body: "hi", Wake: true})
		if err != nil {
			t.Fatal(err)
		}
		convs = append(convs, r.ConversationID)
	}
	// A second message in the first conversation queues behind its run.
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "orbit", SpecSHA: "sha-orbit", Body: "more", Wake: true, ConversationID: convs[0]}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	per := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.ClaimRun(ctx, "w")
			if err != nil {
				t.Error(err)
				return
			}
			if c != nil {
				mu.Lock()
				per[c.ConversationID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if per[convs[0]] != 1 || per[convs[1]] != 1 {
		t.Fatalf("claimed per conversation = %v, want one in each", per)
	}
}
