package store_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func request(t *testing.T, s *store.Store) string {
	t.Helper()
	id, _, err := s.CreateTicket(ctx, store.NewTicket{Kind: "request", Title: "t", Body: "please", Agent: "a", Repo: "kensac/orbit", Author: "kanishk"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func status(t *testing.T, s *store.Store, id string) string {
	t.Helper()
	tk, err := s.GetTicket(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return tk.Status
}

func TestCreateTicketStartsItsThread(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id, msg, err := s.CreateTicket(ctx, store.NewTicket{Kind: "request", Title: "t", Body: "please", Agent: "a", Repo: "kensac/orbit", Author: "kanishk"})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := s.TicketMessages(ctx, id)
	if len(m) != 1 || m[0].ID != msg || m[0].Body != "please" {
		t.Fatalf("thread = %+v", m)
	}
	ev, _ := s.TicketEvents(ctx, id)
	if len(ev) != 1 || ev[0].To != "open" || ev[0].From != nil || *ev[0].By != "kanishk" {
		t.Fatalf("events = %+v", ev)
	}
	if _, _, err := s.CreateTicket(ctx, store.NewTicket{Kind: "question", Title: "q", Body: "?", Agent: "a", Repo: "kensac/orbit"}); err == nil {
		t.Fatal("a question with a repository")
	}
	if _, _, err := s.CreateTicket(ctx, store.NewTicket{Kind: "request", Title: "r", Body: "x", Agent: "a"}); err == nil {
		t.Fatal("a request without a repository")
	}
	if _, _, err := s.CreateTicket(ctx, store.NewTicket{Kind: "question", Title: "q", Body: "?", Agent: "ghost"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown agent = %v", err)
	}
}

func TestTicketLifecycleTable(t *testing.T) {
	// Every edge in docs/tickets.md, and a few that must not exist.
	for _, c := range []struct {
		kind, from, to string
		ok             bool
	}{
		{"request", "open", "planned", true}, {"request", "planned", "open", true},
		{"request", "planned", "approved", true}, {"request", "approved", "building", true},
		{"request", "building", "review", true}, {"request", "review", "merging", true},
		{"request", "merging", "merged", true}, {"request", "merged", "done", true},
		{"request", "failed", "approved", true}, {"request", "failed", "open", true},
		{"request", "failed", "merged", true}, {"request", "review", "abandoned", true},
		{"request", "open", "done", false}, {"request", "merged", "abandoned", false},
		{"request", "done", "open", false}, {"request", "open", "building", false},
		{"question", "open", "done", true}, {"question", "open", "planned", false},
	} {
		if got := store.CanTransition(c.kind, c.from, c.to); got != c.ok {
			t.Errorf("%s %s -> %s = %v", c.kind, c.from, c.to, got)
		}
	}
}

func TestPlanApprovalNamesTheNewestVersion(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := request(t, s)
	if err := s.TransitionTicket(ctx, id, "planned", "x", store.TicketUpdate{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("planned without a plan = %v", err)
	}
	if err := s.ApprovePlan(ctx, id, 1, "kanishk"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("approve with no plan = %v", err)
	}
	v1, err := s.ProposePlan(ctx, id, "", "plan one")
	if err != nil || v1 != 1 || status(t, s, id) != "planned" {
		t.Fatalf("plan 1 = %d %v", v1, err)
	}
	v2, _ := s.ProposePlan(ctx, id, "", "plan two")
	if err := s.ApprovePlan(ctx, id, v1, "kanishk"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("approving a superseded plan = %v", err)
	}
	if err := s.ApprovePlan(ctx, id, v2, "kanishk"); err != nil {
		t.Fatal(err)
	}
	tk, _ := s.GetTicket(ctx, id)
	if tk.Status != "approved" || *tk.ApprovedPlan != 2 {
		t.Fatalf("ticket = %+v", tk)
	}
	if _, err := s.ProposePlan(ctx, id, "", "late"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("plan after approval = %v", err)
	}
	plans, _ := s.TicketPlans(ctx, id)
	if len(plans) != 2 {
		t.Fatalf("plans kept = %d", len(plans))
	}
}

func TestReplyToAPlanReopensTheTicket(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := request(t, s)
	_, _ = s.ProposePlan(ctx, id, "", "plan")
	if _, agent, err := s.SendToTicket(ctx, id, "not quite", "kanishk"); err != nil || agent != "a" {
		t.Fatal(err)
	}
	if st := status(t, s, id); st != "open" {
		t.Fatalf("status after reply = %s", st)
	}
	_ = s.TransitionTicket(ctx, id, "abandoned", "kanishk", store.TicketUpdate{Reason: "changed my mind"})
	if _, _, err := s.SendToTicket(ctx, id, "more", "kanishk"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("message to an abandoned ticket = %v", err)
	}
	ev, _ := s.TicketEvents(ctx, id)
	var d map[string]any
	_ = json.Unmarshal(ev[len(ev)-1].Detail, &d)
	if ev[len(ev)-1].To != "abandoned" || d["reason"] != "changed my mind" {
		t.Fatalf("last event = %+v %s", ev[len(ev)-1], ev[len(ev)-1].Detail)
	}
}

func TestFailureKeepsItsErrorUntilRetried(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := request(t, s)
	_, _ = s.ProposePlan(ctx, id, "", "plan")
	_ = s.ApprovePlan(ctx, id, 1, "kanishk")
	for _, st := range []string{"building"} {
		if err := s.TransitionTicket(ctx, id, st, "orbit", store.TicketUpdate{BaseSHA: "abc"}); err != nil {
			t.Fatal(err)
		}
	}
	pr := 41
	if err := s.TransitionTicket(ctx, id, "review", "orbit", store.TicketUpdate{Branch: "orbit/x", PRNumber: &pr, HeadSHA: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.TransitionTicket(ctx, id, "failed", "orbit", store.TicketUpdate{Error: "check concluded failure"}); err != nil {
		t.Fatal(err)
	}
	tk, _ := s.GetTicket(ctx, id)
	if *tk.Error != "check concluded failure" || *tk.PRNumber != 41 || *tk.BaseSHA != "abc" || *tk.Branch != "orbit/x" {
		t.Fatalf("failed ticket = %+v", tk)
	}
	if err := s.TransitionTicket(ctx, id, "approved", "kanishk", store.TicketUpdate{}); err != nil {
		t.Fatal(err)
	}
	tk, _ = s.GetTicket(ctx, id)
	if tk.Error != nil || *tk.PRNumber != 41 {
		t.Fatalf("retried ticket = %+v", tk)
	}
	if err := s.TransitionTicket(ctx, id, "done", "x", store.TicketUpdate{}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("approved -> done = %v", err)
	}
}

func TestTicketRunsReadOnlyTheirThread(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	one := request(t, s)
	two := request(t, s)
	if _, err := s.SendToAgent(ctx, "a", "general", "kanishk", nil); err != nil {
		t.Fatal(err)
	}
	read := func(ticket *string) []string {
		t.Helper()
		id, err := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: "sha-a", Trigger: "message", TicketID: ticket})
		if err != nil {
			t.Fatal(err)
		}
		c, err := s.ClaimRun(ctx, "w")
		if err != nil || c.ID != id {
			t.Fatalf("claim %v %v", c, err)
		}
		rc, err := s.StartRunContext(ctx, id, "a")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordOutcome(ctx, id, "a", store.RunResult{Status: "succeeded"}, store.Outcome{Questions: []string{"which?"}}); err != nil {
			t.Fatal(err)
		}
		var bodies []string
		for _, m := range rc.Messages {
			bodies = append(bodies, m.Body)
		}
		return bodies
	}
	if got := read(&one); len(got) != 1 || got[0] != "please" {
		t.Fatalf("ticket one read %v", got)
	}
	if got := read(nil); len(got) != 1 || got[0] != "general" {
		t.Fatalf("general run read %v", got)
	}
	// The question ticket one's run asked stays in ticket one's thread.
	m, _ := s.TicketMessages(ctx, one)
	if len(m) != 2 || m[1].Direction != "from_agent" {
		t.Fatalf("ticket one thread = %+v", m)
	}
	// A message on ticket two neither reaches ticket one nor closes its
	// question.
	if _, _, err := s.SendToTicket(ctx, two, "second", "kanishk"); err != nil {
		t.Fatal(err)
	}
	if got := read(&two); len(got) != 2 {
		t.Fatalf("ticket two read %v", got)
	}
	runs, _ := s.TicketRuns(ctx, one)
	if len(runs) != 1 {
		t.Fatalf("ticket one runs = %d", len(runs))
	}
	agents, _ := s.ListAgents(ctx)
	// Open questions: ticket one's and the general one; ticket two's
	// question is open too (nothing answered it).
	if agents[0].Unread != 3 {
		t.Fatalf("open questions = %d", agents[0].Unread)
	}
}
