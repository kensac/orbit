package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/store"
)

// Tickets (docs/tickets.md). Every state change is a guarded transition in
// the store; a 409 means the ticket was no longer where the request assumed.
// The person is the verified token's, as for messages and decisions; body
// author and by fields count only with the operator token.

func decodeBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if r.ContentLength == 0 {
		return true
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(into); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

// actor is who a write is for: the token's person, or with the operator
// token whoever the body names, else "operator".
func actor(r *http.Request, named string) string {
	if p := principalOf(r); p.Person != "" {
		return p.Person
	}
	if named != "" {
		return named
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
	}
	if !decodeBody(w, r, &body) {
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
	wake := a.MessagesTrigger
	if body.Wake != nil {
		wake = *body.Wake
	}
	res, replayed, err := s.Store.CreateTicket(r.Context(), store.NewTicket{Kind: body.Kind, Title: body.Title, Body: body.Body,
		Agent: a.Name, SpecSHA: a.SHA, Repo: body.Repo, Author: actor(r, body.Author), Wake: wake, ClientID: body.ClientRequestID})
	if err == nil && res.RunID != "" && !replayed {
		s.Wake()
	}
	respondStatus(w, http.StatusCreated, res, err)
}

func (s *Server) listTickets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.TicketFilter{Status: q.Get("status"), Agent: q.Get("agent"), Limit: intParam(r, "limit")}
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
	respond(w, map[string]any{"ticket": t, "plans": orEmpty(plans), "events": orEmpty(events)}, err)
}

// ticketThread is a ticket's thread, served exactly as an agent's.
func (s *Server) ticketThread(w http.ResponseWriter, r *http.Request) {
	servePage(w, r, func(before *store.ThreadCursor, limit int) (*store.ThreadPage, error) {
		return s.Store.TicketThread(r.Context(), r.PathValue("id"), before, limit)
	})
}

func (s *Server) ticketMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Body            string  `json:"body"`
		ReplyTo         *string `json:"reply_to"`
		Author          string  `json:"author"`
		Wake            *bool   `json:"wake"`
		ClientMessageID string  `json:"client_message_id"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		writeErr(w, http.StatusBadRequest, "body is required")
		return
	}
	t, err := s.Store.GetTicket(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	a, ok := s.Engine.Current().Config.Agents[t.Agent]
	if !ok {
		// Nothing could answer: the agent left the configuration.
		writeErr(w, http.StatusConflict, "ticket's agent "+strconv.Quote(t.Agent)+" is not configured")
		return
	}
	wake := a.MessagesTrigger
	if body.Wake != nil {
		wake = *body.Wake
	}
	res, replayed, err := s.Store.PostToTicket(r.Context(), store.TicketPost{TicketID: t.ID, Agent: a.Name, SpecSHA: a.SHA,
		Body: body.Body, Author: actor(r, body.Author), ReplyTo: body.ReplyTo, Wake: wake, ClientID: body.ClientMessageID})
	if err == nil && res.RunID != "" && !replayed {
		s.Wake()
	}
	respondStatus(w, http.StatusCreated, res, err)
}

func (s *Server) approvePlan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		By              string `json:"by"`
		ClientRequestID string `json:"client_request_id"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || version < 1 {
		writeErr(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	_, err = s.Store.ApprovePlan(r.Context(), r.PathValue("id"), version, actor(r, body.By), body.ClientRequestID)
	respond(w, map[string]any{"ok": err == nil}, err)
}

// ticketMove serves the transitions a person makes directly.
func (s *Server) ticketMove(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			By     string `json:"by"`
			Reason string `json:"reason"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		err := s.Store.TransitionTicket(r.Context(), r.PathValue("id"), to, actor(r, body.By), store.TicketUpdate{Reason: body.Reason})
		respond(w, map[string]any{"ok": err == nil}, err)
	}
}
