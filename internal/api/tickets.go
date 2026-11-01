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

// wakeTicket enqueues a run on the ticket's thread when wake (or, unset, the
// agent's wake_on_message) says so, and returns its id.
func (s *Server) wakeTicket(r *http.Request, agent, ticketID string, wake *bool) (string, error) {
	a, ok := s.Engine.Current().Config.Agents[agent]
	if !ok {
		// The agent was removed from the configuration: the message is
		// kept, and nothing can answer it until it is back.
		return "", nil
	}
	w := a.MessagesTrigger
	if wake != nil {
		w = *wake
	}
	if !w {
		return "", nil
	}
	id, err := s.Store.EnqueueRun(r.Context(), store.NewRun{Agent: a.Name, SpecSHA: a.SHA, Trigger: "message", TicketID: &ticketID})
	if err == nil {
		s.Wake()
	}
	return id, err
}

func (s *Server) createTicket(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string `json:"kind"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Agent  string `json:"agent"`
		Repo   string `json:"repo"`
		Author string `json:"author"`
		Wake   *bool  `json:"wake"`
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
	if _, ok := cfg.Agents[body.Agent]; !ok {
		writeErr(w, http.StatusBadRequest, "agent "+strconv.Quote(body.Agent)+" is not configured")
		return
	}
	if body.Kind == "request" && !cfg.Deployment.AllowsRepo(body.Repo) {
		writeErr(w, http.StatusBadRequest, "repo "+strconv.Quote(body.Repo)+" is not in github.repositories")
		return
	}
	id, msg, err := s.Store.CreateTicket(r.Context(), store.NewTicket{Kind: body.Kind, Title: body.Title, Body: body.Body,
		Agent: body.Agent, Repo: body.Repo, Author: body.Author})
	if err != nil {
		respond(w, nil, err)
		return
	}
	resp := map[string]any{"ticket_id": id, "message_id": msg}
	runID, err := s.wakeTicket(r, body.Agent, id, body.Wake)
	if runID != "" {
		resp["run_id"] = runID
	}
	respondStatus(w, http.StatusCreated, resp, err)
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
	if err != nil {
		respond(w, nil, err)
		return
	}
	msgs, err := s.Store.TicketMessages(r.Context(), id)
	if err != nil {
		respond(w, nil, err)
		return
	}
	runs, err := s.Store.TicketRuns(r.Context(), id)
	respond(w, map[string]any{"ticket": t, "plans": orEmpty(plans), "events": orEmpty(events),
		"messages": orEmpty(msgs), "runs": orEmpty(runs)}, err)
}

func (s *Server) ticketMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Body   string `json:"body"`
		Author string `json:"author"`
		Wake   *bool  `json:"wake"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Body) == "" {
		writeErr(w, http.StatusBadRequest, "body is required")
		return
	}
	id := r.PathValue("id")
	msg, agent, err := s.Store.SendToTicket(r.Context(), id, body.Body, body.Author)
	if err != nil {
		respond(w, nil, err)
		return
	}
	resp := map[string]any{"message_id": msg}
	runID, err := s.wakeTicket(r, agent, id, body.Wake)
	if runID != "" {
		resp["run_id"] = runID
	}
	respondStatus(w, http.StatusCreated, resp, err)
}

func (s *Server) approvePlan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		By string `json:"by"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	version, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || version < 1 {
		writeErr(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	err = s.Store.ApprovePlan(r.Context(), r.PathValue("id"), version, orDefault(body.By, "api"))
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
		err := s.Store.TransitionTicket(r.Context(), r.PathValue("id"), to, orDefault(body.By, "api"), store.TicketUpdate{Reason: body.Reason})
		respond(w, map[string]any{"ok": err == nil}, err)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
