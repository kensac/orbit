package store_test

import (
	"encoding/json"
	"testing"

	agentstatus "github.com/kensac/orbit/internal/status"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// The agent status every listing derives, through one run's life and the
// question it leaves open after it.
func TestAgentStatusAcrossListings(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	conv := defaultConv(t, s, "a")
	type view struct {
		state agentstatus.State
		kind  agentstatus.Kind
		src   string
	}
	check := func(step string, want view, runID string) {
		t.Helper()
		got := map[string]agentstatus.Status{}
		agents, err := s.ListAgents(ctx)
		if err != nil || len(agents) != 1 {
			t.Fatalf("%s: agents %v %v", step, agents, err)
		}
		got["agent"] = agents[0].AgentStatus
		c, err := s.GetConversation(ctx, conv)
		if err != nil {
			t.Fatal(err)
		}
		got["conversation"] = c.AgentStatus
		page, err := s.ListConversations(ctx, "a", false, nil, 10)
		if err != nil || len(page.Conversations) != 1 {
			t.Fatalf("%s: conversations %v %v", step, page, err)
		}
		got["conversation list"] = page.Conversations[0].AgentStatus
		for where, st := range got {
			if st.State != want.state || st.Kind != want.kind || st.Source != want.src || st.RunID != runID {
				t.Errorf("%s: %s = %+v, want %+v run %q", step, where, st, want, runID)
			}
		}
	}
	check("no run", view{agentstatus.Idle, "", agentstatus.SourceRun}, "")

	r1 := manual(t, s, "a")
	check("queued", view{agentstatus.Queued, "", agentstatus.SourceRun}, r1)
	c, _ := s.ClaimRun(ctx, "w")
	check("running", view{agentstatus.Working, "", agentstatus.SourceRun}, r1)

	snap, _ := json.Marshal(agentstatus.Snapshot{State: agentstatus.Waiting, Kind: agentstatus.Permission, Detail: "Write",
		Event: "PermissionRequest", Attempt: c.Attempt})
	if err := s.SetHookStatus(ctx, r1, c.Attempt, snap); err != nil {
		t.Fatal(err)
	}
	check("a hook waits", view{agentstatus.Waiting, agentstatus.Permission, agentstatus.SourceHooks}, r1)
	h, err := s.Health(ctx)
	if err != nil || len(h.Running) != 1 || h.Running[0].AgentStatus.State != agentstatus.Waiting || h.Running[0].AgentStatus.Detail != "Write" {
		t.Fatalf("health running = %+v %v", h.Running, err)
	}
	run, _ := s.GetRun(ctx, r1)
	tr, _ := s.ThreadRunByID(ctx, r1)
	if run.AgentStatus.State != agentstatus.Waiting || tr.AgentStatus.State != agentstatus.Waiting || tr.AgentStatus.Source != agentstatus.SourceHooks {
		t.Fatalf("run %+v, thread run %+v", run.AgentStatus, tr.AgentStatus)
	}
	if err := s.SetHookStatus(ctx, r1, c.Attempt+1, snap); err == nil {
		t.Fatal("another attempt's report was stored")
	}

	if _, err := s.RecordOutcome(ctx, r1, "a", store.RunResult{Attempt: c.Attempt, Status: "succeeded"},
		store.Outcome{Questions: []store.Question{{Body: "Which card?"}}}); err != nil {
		t.Fatal(err)
	}
	check("a question open", view{agentstatus.Waiting, agentstatus.Question, agentstatus.SourceRun}, r1)
	run, _ = s.GetRun(ctx, r1)
	if run.AgentStatus.State != agentstatus.Waiting || run.AgentStatus.Kind != agentstatus.Question {
		t.Fatalf("run = %+v", run.AgentStatus)
	}

	// A later run in the conversation finishes; nobody answered, so the
	// agent still waits on its person, on the newest run.
	r2 := manual(t, s, "a")
	c2, _ := s.ClaimRun(ctx, "w")
	check("running past an open question", view{agentstatus.Working, "", agentstatus.SourceRun}, r2)
	if err := s.FinishRun(ctx, r2, store.RunResult{Attempt: c2.Attempt, Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	check("finished past an open question", view{agentstatus.Waiting, agentstatus.Question, agentstatus.SourceRun}, r2)
	if run, _ := s.GetRun(ctx, r2); run.AgentStatus.State != agentstatus.Done {
		t.Fatalf("r2 itself is done: %+v", run.AgentStatus)
	}

	// A reply closes the question.
	if _, err := s.SendToAgent(ctx, "a", "the blue one", "k", nil); err != nil {
		t.Fatal(err)
	}
	check("answered", view{agentstatus.Done, "", agentstatus.SourceRun}, r2)
}
