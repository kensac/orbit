package store_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func newRequest(clientID string) store.NewTicket {
	return store.NewTicket{Kind: "request", Title: "Add a flag", Body: "please", Agent: "a", SpecSHA: "sha-a",
		Repo: "kensac/orbit", Author: "kanishk", ClientID: clientID}
}

func request(t *testing.T, s *store.Store) store.TicketCreated {
	t.Helper()
	r, _, err := s.CreateTicket(ctx, newRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func status(t *testing.T, s *store.Store, id string) string {
	t.Helper()
	tk, err := s.GetTicket(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return tk.Status
}

func TestATicketIsAConversation(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	nt := newRequest("c-1")
	nt.Wake = true
	r, replayed, err := s.CreateTicket(ctx, nt)
	if err != nil || replayed || r.RunID == "" || r.ConversationID == "" {
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
	if tickets, _ := s.ListTickets(ctx, store.TicketFilter{}); len(tickets) != 1 {
		t.Fatalf("tickets = %d, want one despite the retry", len(tickets))
	}
	tk, _ := s.GetTicket(ctx, r.TicketID)
	if tk.ConversationID != r.ConversationID {
		t.Fatalf("ticket = %+v", tk)
	}
	c, err := s.GetConversation(ctx, r.ConversationID)
	if err != nil || c.Title != "Add a flag" || c.TicketID == nil || *c.TicketID != r.TicketID {
		t.Fatalf("conversation = %+v %v", c, err)
	}
	page, err := s.ConversationThread(ctx, r.ConversationID, nil, 0)
	if err != nil || len(page.Items) != 2 || page.Items[1].Message == nil || page.Items[1].Message.Body != "please" {
		t.Fatalf("thread = %+v %v", page, err)
	}
	// The run it woke is in the ticket's conversation.
	cl, _ := s.ClaimRun(ctx, "w")
	if cl.ID != r.RunID || cl.ConversationID != r.ConversationID {
		t.Fatalf("claimed %+v", cl)
	}
	ev, _ := s.TicketEvents(ctx, r.TicketID)
	if len(ev) != 1 || ev[0].To != "open" || ev[0].From != nil || *ev[0].By != "kanishk" {
		t.Fatalf("events = %+v", ev)
	}
	for _, bad := range []store.NewTicket{
		{Kind: "question", Title: "q", Body: "?", Agent: "a", Repo: "kensac/orbit"},
		{Kind: "request", Title: "r", Body: "x", Agent: "a"},
		{Kind: "bug", Title: "r", Body: "x", Agent: "a"},
	} {
		if _, _, err := s.CreateTicket(ctx, bad); !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("%+v = %v", bad, err)
		}
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
	id := request(t, s).TicketID
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

func TestAMessageInATicketsConversationMovesTheTicket(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	r := request(t, s)
	_, _ = s.ProposePlan(ctx, r.TicketID, "", "plan")
	post := store.PostMessage{Agent: "a", SpecSHA: "sha-a", Body: "not quite", Author: "kanishk", ConversationID: r.ConversationID}
	if _, _, err := s.PostMessageOnce(ctx, post); err != nil {
		t.Fatal(err)
	}
	if st := status(t, s, r.TicketID); st != "open" {
		t.Fatalf("status after reply = %s", st)
	}
	_ = s.TransitionTicket(ctx, r.TicketID, "abandoned", "kanishk", store.TicketUpdate{Reason: "changed my mind"})
	if _, _, err := s.PostMessageOnce(ctx, post); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("message to an abandoned ticket = %v", err)
	}
	ev, _ := s.TicketEvents(ctx, r.TicketID)
	var d map[string]any
	_ = json.Unmarshal(ev[len(ev)-1].Detail, &d)
	if ev[len(ev)-1].To != "abandoned" || d["reason"] != "changed my mind" {
		t.Fatalf("last event = %+v %s", ev[len(ev)-1], ev[len(ev)-1].Detail)
	}
	// Other conversations are untouched by tickets.
	if _, _, err := s.PostMessageOnce(ctx, store.PostMessage{Agent: "a", Body: "general"}); err != nil {
		t.Fatal(err)
	}
}

func TestFailureKeepsItsErrorUntilRetried(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	id := request(t, s).TicketID
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
