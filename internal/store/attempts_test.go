package store_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestAnAttemptThatLostItsClaimCanWriteNothing(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := manual(t, s, "a")
	old, _ := s.ClaimRun(ctx, "old/0")
	if err := s.AttachSession(ctx, id, old.Attempt, "s1"); err != nil {
		t.Fatal(err)
	}
	// Its heartbeats stop landing; a reaper requeues it and another
	// instance claims it.
	if _, err := s.Pool.Exec(ctx, `UPDATE runs SET heartbeat_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReapStale(ctx, time.Minute); err != nil {
		t.Fatal(err)
	}
	cur, _ := s.ClaimRun(ctx, "new/0")
	if cur == nil || cur.Attempt != 2 {
		t.Fatalf("reclaim = %+v", cur)
	}
	for name, err := range map[string]error{
		"heartbeat":  s.Heartbeat(ctx, id, old.Attempt),
		"set runner": s.SetRunner(ctx, id, old.Attempt, "claude-code", "", "sha-a"),
		"session":    s.SetSession(ctx, id, old.Attempt, "fresh", "", 1),
		"attach":     s.AttachSession(ctx, id, old.Attempt, "s9"),
		"hand back":  s.HandBack(ctx, id, old.Attempt, nil, nil),
		"finish":     s.FinishRun(ctx, id, store.RunResult{Attempt: old.Attempt, Status: "succeeded"}),
	} {
		if !errors.Is(err, store.ErrConflict) {
			t.Errorf("%s by the superseded attempt = %v, want ErrConflict", name, err)
		}
	}
	if err := s.Heartbeat(ctx, id, cur.Attempt); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishRun(ctx, id, store.RunResult{Attempt: cur.Attempt, Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	at, _ := s.ListAttempts(ctx, id)
	if len(at) != 2 || *at[0].Outcome != "lost" || *at[0].SessionID != "s1" || at[0].Worker != "old/0" ||
		*at[1].Outcome != "succeeded" || at[1].Worker != "new/0" {
		t.Fatalf("attempts = %+v %+v", at[0], at[1])
	}
}

func TestTwoInstancesClaimEachRunOnce(t *testing.T) {
	a := testdb.New(t)
	// The second instance: its own pool on the same database.
	b, err := store.Open(ctx, a.Pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var agents []string
	for i := 0; i < 12; i++ {
		agents = append(agents, fmt.Sprintf("a%d", i))
	}
	seed(t, a, agents...)
	for _, n := range agents {
		manual(t, a, n)
		manual(t, a, n)
	}
	var mu sync.Mutex
	seen := map[string]string{}
	running := map[string]int{}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, name := a, fmt.Sprintf("a/%d", i)
			if i%2 == 1 {
				st, name = b, fmt.Sprintf("b/%d", i)
			}
			for {
				c, err := st.ClaimRun(ctx, name)
				if err != nil {
					t.Error(err)
					return
				}
				if c == nil {
					// Nothing claimable now; done only when nothing is queued.
					var left int
					if err := st.Pool.QueryRow(ctx, `SELECT count(*) FROM runs WHERE status = 'queued'`).Scan(&left); err != nil || left == 0 {
						return
					}
					time.Sleep(time.Millisecond)
					continue
				}
				mu.Lock()
				if prev, ok := seen[c.ID]; ok {
					t.Errorf("run %s claimed by %s and %s", c.ID, prev, name)
				}
				seen[c.ID] = name
				running[c.Agent]++
				if running[c.Agent] > 1 {
					t.Errorf("agent %s has two runs running", c.Agent)
				}
				mu.Unlock()
				time.Sleep(time.Millisecond)
				mu.Lock()
				running[c.Agent]--
				mu.Unlock()
				if err := st.FinishRun(ctx, c.ID, store.RunResult{Attempt: c.Attempt, Status: "succeeded"}); err != nil {
					t.Error(err)
				}
			}
		}(i)
	}
	wg.Wait()
	byInstance := map[byte]int{}
	for _, w := range seen {
		byInstance[w[0]]++
	}
	if len(seen) != 24 || byInstance['a'] == 0 || byInstance['b'] == 0 {
		t.Fatalf("claimed %d runs, by instance %v; want all 24, split", len(seen), byInstance)
	}
}

func TestARunCostsEveryAttempt(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	interrupted, total := 0.1, 0.1
	if err := s.HandBack(ctx, id, c.Attempt, &interrupted, &total); err != nil {
		t.Fatal(err)
	}
	c, _ = s.ClaimRun(ctx, "w")
	if err := s.SetRecovery(ctx, id, c.Attempt, 1, ""); err != nil {
		t.Fatal(err)
	}
	rest, all := 0.25, 0.35
	if err := s.FinishRun(ctx, id, store.RunResult{Attempt: c.Attempt, Status: "succeeded", CostUSD: &rest, SessionCostUSD: &all}); err != nil {
		t.Fatal(err)
	}
	run, _ := s.GetRun(ctx, id)
	if run.CostUSD == nil || *run.CostUSD != 0.35 {
		t.Fatalf("run cost = %v, want 0.35", run.CostUSD)
	}
	at, _ := s.ListAttempts(ctx, id)
	if *at[0].Outcome != "handed_back" || *at[0].CostUSD != 0.1 || *at[1].RecoveredFrom != 1 || *at[1].CostUSD != 0.25 {
		t.Fatalf("attempts = %+v %+v", at[0], at[1])
	}
}
