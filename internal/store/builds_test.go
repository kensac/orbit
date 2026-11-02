package store_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// approved files a request with agent a and approves its first plan.
func approved(t *testing.T, s *store.Store) store.TicketCreated {
	t.Helper()
	r := request(t, s)
	if _, err := s.ProposePlan(ctx, r.TicketID, "", "the plan"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApprovePlan(ctx, r.TicketID, 1, "kanishk", ""); err != nil {
		t.Fatal(err)
	}
	return r
}

func claimBuild(t *testing.T, s *store.Store, runID string) *store.ClaimedRun {
	t.Helper()
	c, err := s.ClaimRun(ctx, "w")
	if err != nil || c == nil || c.ID != runID || c.Trigger != store.TriggerBuild {
		t.Fatalf("claim = %+v %v, want build run %s", c, err, runID)
	}
	return c
}

func TestABuildMovesItsTicketWithItsRun(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a", "builder")
	r := approved(t, s)
	if _, err := s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "builder", SpecSHA: "sha-builder", Purpose: "fix",
		Failure: json.RawMessage(`[]`)}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a fix of an approved ticket = %v", err)
	}
	run, err := s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: "build"})
	if err != nil {
		t.Fatal(err)
	}
	if status(t, s, r.TicketID) != store.TicketBuilding {
		t.Fatal("not building")
	}
	// The build agent is the ticket's agent: the run is in the ticket's
	// conversation.
	c := claimBuild(t, s, run)
	if c.ConversationID != r.ConversationID {
		t.Fatalf("conversation %s, want the ticket's %s", c.ConversationID, r.ConversationID)
	}
	b, err := s.BuildOfRun(ctx, run)
	if err != nil || b.Plan != "the plan" || b.PlanVersion != 1 || b.Purpose != "build" || b.RunStatus != "running" {
		t.Fatalf("build = %+v %v", b, err)
	}
	// Decisions and sandboxes are fenced by the attempt.
	if err := s.RecordDecision(ctx, store.Decision{RunID: run, Attempt: c.Attempt, RequestID: "r1", Tool: "Write",
		Input: json.RawMessage(`{"file_path":"/work/x"}`), Allowed: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDecision(ctx, store.Decision{RunID: run, Attempt: c.Attempt + 1, RequestID: "r2", Tool: "Write",
		Input: json.RawMessage(`{}`)}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a decision of another attempt = %v", err)
	}
	if err := s.RecordSandbox(ctx, run, c.Attempt, "sbx1"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSandbox(ctx, run, c.Attempt+1, "sbx2"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a sandbox of another attempt = %v", err)
	}
	if open, _ := s.OpenSandboxes(ctx, run); len(open) != 1 || open[0] != "sbx1" {
		t.Fatalf("open = %v", open)
	}
	_ = s.SandboxDestroyed(ctx, "sbx1")
	if open, _ := s.OpenSandboxes(ctx, run); len(open) != 0 {
		t.Fatalf("open after destroy = %v", open)
	}
	pr := 7
	if err := s.FinishBuild(ctx, run, store.RunResult{Attempt: c.Attempt, Summary: "done"},
		store.TicketUpdate{BaseSHA: "b", Branch: "orbit/x", PRNumber: &pr, HeadSHA: "h"}); err != nil {
		t.Fatal(err)
	}
	tk, _ := s.GetTicket(ctx, r.TicketID)
	if tk.Status != store.TicketReview || *tk.PRNumber != 7 || *tk.HeadSHA != "h" || *tk.Branch != "orbit/x" {
		t.Fatalf("ticket = %+v", tk)
	}
	if ds, _ := s.Decisions(ctx, run); len(ds) != 1 || !ds[0].Allowed {
		t.Fatalf("decisions = %+v", ds)
	}

	// A fix from review, in the build agent's default conversation when the
	// build agent is another agent; a failed fix fails the ticket.
	fix, err := s.StartBuild(ctx, store.StartBuild{TicketID: r.TicketID, Agent: "builder", SpecSHA: "sha-builder", Purpose: "fix",
		Failure: json.RawMessage(`[{"name":"check"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	fc := claimBuild(t, s, fix)
	if fc.ConversationID == r.ConversationID {
		t.Fatal("another agent's run landed in the ticket's conversation")
	}
	if n, _ := s.FixesSinceBuild(ctx, r.TicketID); n != 1 {
		t.Fatalf("fixes = %d", n)
	}
	if err := s.FailBuild(ctx, fix, store.RunResult{Attempt: fc.Attempt, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	tk, _ = s.GetTicket(ctx, r.TicketID)
	if tk.Status != store.TicketFailed || *tk.Error != "build: boom" {
		t.Fatalf("ticket = %+v", tk)
	}
	if lb, _ := s.LatestBuild(ctx, r.TicketID); lb.RunID != fix || lb.RunStatus != "failed" {
		t.Fatalf("latest = %+v", lb)
	}
}

func TestOneBuildAndOneMergePerRepository(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	one, two := approved(t, s), approved(t, s)
	run, err := s.StartBuild(ctx, store.StartBuild{TicketID: one.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: "build"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.StartBuild(ctx, store.StartBuild{TicketID: two.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: "build"}); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("second build = %v", err)
	}
	if status(t, s, two.TicketID) != store.TicketApproved {
		t.Fatal("a busy build must leave the ticket approved, with no run")
	}
	c := claimBuild(t, s, run)
	pr := 7
	_ = s.FinishBuild(ctx, run, store.RunResult{Attempt: c.Attempt}, store.TicketUpdate{PRNumber: &pr, HeadSHA: "h1"})
	run2, err := s.StartBuild(ctx, store.StartBuild{TicketID: two.TicketID, Agent: "a", SpecSHA: "sha-a", Purpose: "build"})
	if err != nil {
		t.Fatal(err)
	}
	c2 := claimBuild(t, s, run2)
	// A build delivered without the model (an earlier attempt's push) is
	// skipped, naming its gate, and moves its ticket all the same.
	_ = s.FinishBuild(ctx, run2, store.RunResult{Attempt: c2.Attempt, Gate: "pushed"}, store.TicketUpdate{PRNumber: &pr, HeadSHA: "h2"})
	if r, _ := s.GetRun(ctx, run2); r.Status != store.StatusSkipped || r.Gate == nil || *r.Gate != "pushed" {
		t.Fatalf("redelivered build = %s %v, want skipped by pushed", r.Status, r.Gate)
	}
	if status(t, s, two.TicketID) != store.TicketReview {
		t.Fatal("a skipped build must still move its ticket to review")
	}

	m := store.MergeProposal{TicketID: one.TicketID, RunID: run, Agent: "a", HeadSHA: "stale", Params: json.RawMessage(`{}`), Rule: "approve"}
	if _, err := s.ProposeMerge(ctx, m); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a merge for a head that moved = %v", err)
	}
	m.HeadSHA = "h1"
	id, err := s.ProposeMerge(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := s.GetAction(ctx, id); a.Status != "pending" || a.Type != "github:merge" || a.RunID != run {
		t.Fatalf("action = %+v", a)
	}
	tk, _ := s.GetTicket(ctx, one.TicketID)
	if tk.Status != store.TicketMerging || *tk.MergeActionID != id {
		t.Fatalf("ticket = %+v", tk)
	}
	if _, err := s.ProposeMerge(ctx, store.MergeProposal{TicketID: two.TicketID, RunID: run2, Agent: "a", HeadSHA: "h2",
		Params: json.RawMessage(`{}`), Rule: "auto"}); !errors.Is(err, store.ErrBusy) {
		t.Fatalf("second merge = %v", err)
	}
	if status(t, s, two.TicketID) != store.TicketReview {
		t.Fatal("a busy merge waits in review")
	}
	if _, err := s.ProposeMerge(ctx, store.MergeProposal{TicketID: two.TicketID, Rule: "deny"}); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("a denied merge is never proposed: %v", err)
	}
	inFlight, _ := s.TicketsInFlight(ctx)
	if len(inFlight) != 2 {
		t.Fatalf("in flight = %d", len(inFlight))
	}
}

func TestAPlanIsStoredWithItsRunsOutcome(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r := request(t, s)
	run := manual(t, s, "a")
	c, _ := s.ClaimRun(ctx, "w")
	p := &store.PlanProposal{TicketID: r.TicketID, Body: "do it"}
	if _, err := s.RecordOutcome(ctx, run, "a", store.RunResult{Attempt: c.Attempt, Status: "succeeded"}, store.Outcome{Plan: p}); err != nil {
		t.Fatal(err)
	}
	if p.Version != 1 || status(t, s, r.TicketID) != store.TicketPlanned {
		t.Fatalf("plan version %d, status %s", p.Version, status(t, s, r.TicketID))
	}
	// A ticket that moved on keeps the run's outcome and drops the plan.
	if err := s.TransitionTicket(ctx, r.TicketID, store.TicketAbandoned, "k", store.TicketUpdate{}); err != nil {
		t.Fatal(err)
	}
	run2 := manual(t, s, "a")
	c2, _ := s.ClaimRun(ctx, "w")
	p2 := &store.PlanProposal{TicketID: r.TicketID, Body: "again"}
	if _, err := s.RecordOutcome(ctx, run2, "a", store.RunResult{Attempt: c2.Attempt, Status: "succeeded"}, store.Outcome{Plan: p2}); err != nil {
		t.Fatal(err)
	}
	if p2.Version != 0 {
		t.Fatalf("a plan for an abandoned ticket was stored as version %d", p2.Version)
	}
	if got, _ := s.GetRun(ctx, run2); got.Status != "succeeded" {
		t.Fatal("the run lost its outcome")
	}
	if msgs, err := s.TicketMessages(ctx, r.TicketID); err != nil || len(msgs) != 1 || msgs[0].Body != "please" {
		t.Fatalf("messages = %+v %v", msgs, err)
	}
	if tk, _ := s.TicketForConversation(ctx, r.ConversationID); tk == nil || tk.ID != r.TicketID {
		t.Fatal("the ticket of its conversation")
	}
}
