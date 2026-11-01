package store_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func newRequest(clientID string) store.NewTicket {
	return store.NewTicket{Kind: "request", Title: "t", Body: "please", Agent: "a", SpecSHA: "sha-a",
		Repo: "kensac/orbit", Author: "kanishk", ClientID: clientID}
}

func request(t *testing.T, s *store.Store) string {
	t.Helper()
	r, _, err := s.CreateTicket(ctx, newRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	return r.TicketID
}

func status(t *testing.T, s *store.Store, id string) string {
	t.Helper()
	tk, err := s.GetTicket(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return tk.Status
}

func TestCreateTicketStartsItsThreadOnce(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	nt := newRequest("c-1")
	nt.Wake = true
	r, replayed, err := s.CreateTicket(ctx, nt)
	if err != nil || replayed || r.RunID == "" {
		t.Fatalf("create = %+v %v %v", r, replayed, err)
	}
	again, replayed, err := s.CreateTicket(ctx, nt)
	if err != nil || !replayed || again != r {
		t.Fatalf("retry = %+v %v %v, want the first result", again, replayed, err)
	}
	nt.Title = "different"
	if _, _, err := s.CreateTicket(ctx, nt); !errors.Is(err, store.ErrReceiptMismatch) {
		t.Fatalf("reused id = %v", err)
	}
	tickets, _ := s.ListTickets(ctx, store.TicketFilter{})
	if len(tickets) != 1 {
		t.Fatalf("tickets = %d, want one despite the retry", len(tickets))
	}
	page, err := s.TicketThread(ctx, r.TicketID, nil, 0)
	if err != nil || len(page.Items) != 2 || page.Items[1].Message == nil || page.Items[1].Message.Body != "please" ||
		page.Items[0].Run == nil || page.Items[0].Run.ID != r.RunID {
		t.Fatalf("thread = %+v %v", page, err)
	}
	ev, _ := s.TicketEvents(ctx, r.TicketID)
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
	if _, err := s.TicketThread(ctx, "00000000-0000-0000-0000-000000000000", nil, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown ticket thread = %v", err)
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
	if _, err := s.ApprovePlan(ctx, id, 1, "kanishk", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("approve with no plan = %v", err)
	}
	v1, err := s.ProposePlan(ctx, id, "", "plan one")
	if err != nil || v1 != 1 || status(t, s, id) != "planned" {
		t.Fatalf("plan 1 = %d %v", v1, err)
	}
	v2, _ := s.ProposePlan(ctx, id, "", "plan two")
	if _, err := s.ApprovePlan(ctx, id, v1, "kanishk", ""); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("approving a superseded plan = %v", err)
	}
	if replayed, err := s.ApprovePlan(ctx, id, v2, "kanishk", "ap-1"); err != nil || replayed {
		t.Fatal(err)
	}
	// A retried approval is a success, not a conflict.
	if replayed, err := s.ApprovePlan(ctx, id, v2, "kanishk", "ap-1"); err != nil || !replayed {
		t.Fatalf("retried approval = %v %v", replayed, err)
	}
	tk, _ := s.GetTicket(ctx, id)
	if tk.Status != "approved" || *tk.ApprovedPlan != 2 {
		t.Fatalf("ticket = %+v", tk)
	}
	if _, err := s.ProposePlan(ctx, id, "", "late"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("plan after approval = %v", err)
	}
	if plans, _ := s.TicketPlans(ctx, id); len(plans) != 2 {
		t.Fatalf("plans kept = %d", len(plans))
	}
}

func TestReplyToAPlanReopensTheTicket(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := request(t, s)
	_, _ = s.ProposePlan(ctx, id, "", "plan")
	post := store.TicketPost{TicketID: id, Agent: "a", SpecSHA: "sha-a", Body: "not quite", Author: "kanishk"}
	if _, _, err := s.PostToTicket(ctx, post); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, id); st != "open" {
		t.Fatalf("status after reply = %s", st)
	}
	wrong := post
	wrong.Agent = "b"
	if _, _, err := s.PostToTicket(ctx, wrong); err == nil {
		t.Fatal("a post naming another agent")
	}
	_ = s.TransitionTicket(ctx, id, "abandoned", "kanishk", store.TicketUpdate{Reason: "changed my mind"})
	if _, _, err := s.PostToTicket(ctx, post); !errors.Is(err, store.ErrConflict) {
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
	_, _ = s.ApprovePlan(ctx, id, 1, "kanishk", "")
	if err := s.TransitionTicket(ctx, id, "building", "orbit", store.TicketUpdate{BaseSHA: "abc"}); err != nil {
		t.Fatal(err)
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

// TestEveryThreadReaderIsScopedByTicket holds the scoping table in
// docs/tickets.md: inbox, questions, conversation, session, continuation and
// the thread page each see one thread only.
func TestEveryThreadReaderIsScopedByTicket(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	one := request(t, s)
	two := request(t, s)
	if _, err := s.SendToAgent(ctx, "a", "general", "kanishk", nil); err != nil {
		t.Fatal(err)
	}
	// run claims and finishes a run on one thread and returns what it read.
	run := func(ticket *string, session string) (string, []string) {
		t.Helper()
		id, err := s.EnqueueRun(ctx, store.NewRun{Agent: "a", SpecSHA: "sha-a", Trigger: "message", TicketID: ticket})
		if err != nil {
			t.Fatal(err)
		}
		c, err := s.ClaimRun(ctx, "w")
		if err != nil || c.ID != id || (c.TicketID == nil) != (ticket == nil) {
			t.Fatalf("claim %+v %v", c, err)
		}
		rc, err := s.StartRunContext(ctx, id, "a")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.RecordOutcome(ctx, id, "a", store.RunResult{Status: "succeeded", Report: "report " + session, SessionID: session},
			store.Outcome{Questions: []string{"which?"}}); err != nil {
			t.Fatal(err)
		}
		var bodies []string
		for _, m := range rc.Messages {
			bodies = append(bodies, m.Body)
		}
		return id, bodies
	}
	oneRun, got := run(&one, "s-one")
	if len(got) != 1 || got[0] != "please" {
		t.Fatalf("ticket one read %v", got)
	}
	generalRun, got := run(nil, "s-general")
	if len(got) != 1 || got[0] != "general" {
		t.Fatalf("general run read %v", got)
	}

	// Sessions: each thread resumes its own.
	if p, _ := s.LatestSession(ctx, "a", &one); p == nil || p.SessionID != "s-one" {
		t.Fatalf("ticket one's session = %+v", p)
	}
	if p, _ := s.LatestSession(ctx, "a", nil); p == nil || p.SessionID != "s-general" {
		t.Fatalf("general session = %+v", p)
	}
	if p, _ := s.LatestSession(ctx, "a", &two); p != nil {
		t.Fatalf("ticket two has no session yet, got %+v", p)
	}

	// A message on ticket two neither reaches ticket one nor closes its
	// question, nor the general one.
	if _, _, err := s.PostToTicket(ctx, store.TicketPost{TicketID: two, Agent: "a", SpecSHA: "sha-a", Body: "second"}); err != nil {
		t.Fatal(err)
	}
	twoRun, got := run(&two, "s-two")
	if len(got) != 2 {
		t.Fatalf("ticket two read %v", got)
	}
	agents, _ := s.ListAgents(ctx)
	if agents[0].Unread != 3 {
		t.Fatalf("open questions = %d, want one per thread", agents[0].Unread)
	}

	// The conversation a later run on ticket one is shown holds only
	// ticket one's turns.
	convo, err := s.Conversation(ctx, "a", twoRun, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range convo {
		if it.RunID == oneRun || it.RunID == generalRun || it.Body == "general" || it.Body == "please" {
			t.Fatalf("ticket two's conversation shows another thread: %+v", it)
		}
	}
	convo, _ = s.Conversation(ctx, "a", generalRun, 50)
	for _, it := range convo {
		if it.RunID == oneRun || it.RunID == twoRun {
			t.Fatalf("the general conversation shows a ticket: %+v", it)
		}
	}

	// A continuation stays in the thread of the run that asked for it.
	cid, queued, err := s.EnqueueContinuation(ctx, store.Continuation{Agent: "a", FromRun: oneRun, After: time.Minute, Note: "n", MaxChain: 3})
	if err != nil || !queued {
		t.Fatal(err)
	}
	if r, _ := s.TicketThread(ctx, one, nil, 0); !hasRun(r, cid) {
		t.Fatal("the continuation is not in ticket one's thread")
	}

	// The thread pages: the general thread holds no ticket items.
	gen, _ := s.Thread(ctx, "a", nil, 0)
	for _, it := range gen.Items {
		if it.Run != nil && (it.Run.ID == oneRun || it.Run.ID == twoRun || it.Run.ID == cid) {
			t.Fatalf("general thread shows ticket run %s", it.Run.ID)
		}
	}
	if !hasRun(gen, generalRun) {
		t.Fatal("general thread lost its run")
	}
	// Paging a ticket's thread with the shared cursor.
	page, _ := s.TicketThread(ctx, two, nil, 2)
	if !page.HasMore || len(page.Items) != 2 {
		t.Fatalf("first page = %+v", page)
	}
	cur, _ := store.ParseThreadCursor(page.NextBefore)
	rest, _ := s.TicketThread(ctx, two, cur, 50)
	if len(rest.Items)+len(page.Items) != 4 || rest.HasMore { // please, second, the run, its question
		t.Fatalf("ticket two thread = %d + %d items", len(page.Items), len(rest.Items))
	}
}

func hasRun(p *store.ThreadPage, id string) bool {
	for _, it := range p.Items {
		if it.Run != nil && it.Run.ID == id {
			return true
		}
	}
	return false
}
