package api

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/usage"
)

// Tickets (docs/tickets.md): a ticket is a conversation with a lifecycle.
// Its thread and messages are its conversation's routes; these are the
// lifecycle. Every state change is a guarded transition in the store; a 409
// means the ticket was no longer where the request assumed. The person is
// the verified token's; body author and by count only with the operator
// token.

// decider is who decides a transition: the token's person, or with the
// operator token whoever the body names, else "operator".
func decider(r *http.Request, named string) string {
	if a := actor(r, named); a != "" {
		return a
	}
	return "operator"
}

func (s *Server) createTicket(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string `json:"kind"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Agent  string `json:"agent"`
		Repo   string `json:"repo"`
		Author string `json:"author"`
		// Wake overrides the agent's wake_on_message for the first message.
		Wake            *bool  `json:"wake"`
		ClientRequestID string `json:"client_request_id"`
		// StackOn files the request on top of another of the same
		// repository (docs/tickets.md, Stacked tickets).
		StackOn string `json:"stack_on"`
		// Race builds the approved plan 2 or 3 times, each lane with its
		// variant (docs/tickets.md, Races).
		Race     int             `json:"race"`
		Variants []store.Variant `json:"variants"`
	}
	if !decode(w, r, 64<<10, &body, bodyOptional) {
		return
	}
	body.Title, body.Body = strings.TrimSpace(body.Title), strings.TrimSpace(body.Body)
	switch {
	case body.Kind != "question" && body.Kind != "request":
		writeErr(w, http.StatusBadRequest, "kind must be question or request")
		return
	case body.Title == "" || body.Body == "":
		writeErr(w, http.StatusBadRequest, "title and body are required")
		return
	case body.Kind == "question" && body.Repo != "":
		writeErr(w, http.StatusBadRequest, "a question does not name a repository")
		return
	}
	cfg := s.Engine.Current().Config
	a, ok := cfg.Agents[body.Agent]
	if !ok {
		writeErr(w, http.StatusBadRequest, "agent "+strconv.Quote(body.Agent)+" is not configured")
		return
	}
	if body.Kind == "request" && !cfg.Deployment.AllowsRepo(body.Repo) {
		writeErr(w, http.StatusBadRequest, "repo "+strconv.Quote(body.Repo)+" is not in github.repositories")
		return
	}
	if body.Race > 0 {
		w8, err := usage.Crossed(r.Context(), s.Store, time.Now())
		if err != nil {
			respond(w, nil, err)
			return
		}
		if w8 != nil {
			writeErr(w, http.StatusConflict, "a race is refused while "+w8.Above()+"; file it without race, or once the window drops")
			return
		}
	}
	wake := a.MessagesTrigger
	if body.Wake != nil {
		wake = *body.Wake
	}
	res, replayed, err := s.Store.CreateTicket(r.Context(), store.NewTicket{Kind: body.Kind, Title: body.Title, Body: body.Body,
		Agent: a.Name, SpecSHA: a.SHA, Repo: body.Repo, Author: actor(r, body.Author), Wake: wake, ClientID: body.ClientRequestID,
		StackOn: body.StackOn, Race: body.Race, Variants: body.Variants})
	if err == nil && res.RunID != "" && !replayed {
		s.Wake()
	}
	respondStatus(w, http.StatusCreated, res, err)
}

func (s *Server) listTickets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TicketFilter{Status: q.Get("status"), Agent: q.Get("agent")}
	if !queryInt(w, r, "limit", &f.Limit) {
		return
	}
	if b := q.Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339Nano, b)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "before: "+err.Error())
			return
		}
		f.Before = &t
	}
	t, err := s.Store.ListTickets(r.Context(), f)
	respond(w, map[string]any{"tickets": orEmpty(t)}, err)
}

func (s *Server) getTicket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	t, err := s.Store.GetTicket(r.Context(), id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	plans, err := s.Store.TicketPlans(r.Context(), id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	events, err := s.Store.TicketEvents(r.Context(), id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	reruns, err := s.Store.TicketReruns(r.Context(), id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	out := map[string]any{"ticket": t, "plans": orEmpty(plans), "events": orEmpty(events), "reruns": orEmpty(reruns)}
	if t.Race != nil {
		lanes, err := s.Store.RaceLanes(r.Context(), id)
		if err != nil {
			respond(w, nil, err)
			return
		}
		out["race"] = raceView(t, lanes)
	}
	respond(w, out, nil)
}

// raceView is a race's comparison: every lane as code scored it, best
// first, and the lane code recommends (the best that delivered a PR).
func raceView(t *store.Ticket, lanes []store.Lane) map[string]any {
	ranked := slices.Clone(lanes)
	slices.SortFunc(ranked, func(a, b store.Lane) int { return a.Rank - b.Rank })
	v := map[string]any{"lanes": ranked}
	if len(ranked) > 0 && ranked[0].Delivered {
		v["recommended"] = ranked[0].Lane
	}
	if t.PickedLane != nil {
		v["picked"] = *t.PickedLane
	}
	return v
}

// pickLane is a person picking one lane of a race (store.PickLane): the
// ticket:pick action is approved and executed by them, never by a rule.
func (s *Server) pickLane(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Lane            string `json:"lane"`
		By              string `json:"by"`
		ClientRequestID string `json:"client_request_id"`
	}
	if !decode(w, r, 4<<10, &body, bodyRequired) {
		return
	}
	id, _, err := s.Store.PickLane(r.Context(), store.Pick{TicketID: r.PathValue("id"), Lane: body.Lane, By: decider(r, body.By),
		ClientID: body.ClientRequestID})
	respond(w, map[string]any{"ok": err == nil, "action_id": id}, err)
}

// rerunChecks records a person's request to rerun a PR's failed checks;
// the ticket loop carries it out on its next tick, with the only token
// that can.
func (s *Server) rerunChecks(w http.ResponseWriter, r *http.Request) {
	var body struct {
		By string `json:"by"`
	}
	if !decode(w, r, 64<<10, &body, bodyOptional) {
		return
	}
	rr, err := s.Store.RequestRerun(r.Context(), r.PathValue("id"), decider(r, body.By))
	respondStatus(w, http.StatusAccepted, map[string]any{"rerun": rr}, err)
}

func (s *Server) approvePlan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		By              string `json:"by"`
		ClientRequestID string `json:"client_request_id"`
	}
	if !decode(w, r, 64<<10, &body, bodyOptional) {
		return
	}
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || version < 1 {
		writeErr(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	_, err = s.Store.ApprovePlan(r.Context(), r.PathValue("id"), version, decider(r, body.By), body.ClientRequestID)
	respond(w, map[string]any{"ok": err == nil}, err)
}

// ticketMove serves the transitions a person makes directly.
func (s *Server) ticketMove(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			By     string `json:"by"`
			Reason string `json:"reason"`
		}
		if !decode(w, r, 64<<10, &body, bodyOptional) {
			return
		}
		err := s.Store.TransitionTicket(r.Context(), r.PathValue("id"), to, decider(r, body.By), store.TicketUpdate{Reason: body.Reason})
		respond(w, map[string]any{"ok": err == nil}, err)
	}
}
