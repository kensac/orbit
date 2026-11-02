package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// Conversations: an agent's threads, as many as a person opens. The agent's
// default conversation is also /agents/{name}/thread and /messages.

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	resp, err := s.conversationPage(r.Context(), r.PathValue("name"), r.URL.Query().Get("archived") == "true",
		r.URL.Query().Get("before"), limit)
	respond(w, resp, err)
}

// conversationPage is a page of an agent's conversations for HTTP and MCP alike; before is the previous page's next_before.
func (s *Server) conversationPage(ctx context.Context, agent string, archived bool, before string, limit int) (map[string]any, error) {
	var c *store.ConversationCursor
	if before != "" {
		var err error
		if c, err = store.ParseConversationCursor(before); err != nil {
			return nil, requestError("before: " + err.Error())
		}
	}
	page, err := s.Store.ListConversations(ctx, agent, archived, c, limit)
	if err != nil {
		return nil, err
	}
	return listPage("conversations", page.Conversations, page.HasMore, page.NextBefore), nil
}

func (s *Server) createConversation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
		// Body is an optional first message; it names the conversation
		// unless Title does, and wakes a run per Wake.
		Body            string `json:"body"`
		Author          string `json:"author"`
		Wake            *bool  `json:"wake"`
		ClientRequestID string `json:"client_request_id"`
		// Agent is POST /conversations's: the agent to talk to, the
		// built-in general agent when empty.
		Agent string `json:"agent"`
	}
	if !decode(w, r, 64<<10, &body, bodyOptional) {
		return
	}
	name := r.PathValue("name")
	if name == "" {
		name = body.Agent
		if name == "" {
			name = spec.BuiltinName
		}
	}
	a, ok := s.Engine.Current().Config.Agents[name]
	if !ok {
		respond(w, nil, store.ErrNotFound)
		return
	}
	wake := a.MessagesTrigger
	if body.Wake != nil {
		wake = *body.Wake
	}
	res, replayed, err := s.Store.CreateConversation(r.Context(), store.NewConversation{
		Agent: a.Name, SpecSHA: a.SHA, Title: body.Title, Body: strings.TrimSpace(body.Body),
		Author: actor(r, body.Author), Wake: wake, ClientID: body.ClientRequestID,
	})
	if err == nil && res.RunID != "" && !replayed {
		s.Wake()
	}
	respondStatus(w, http.StatusCreated, res, err)
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.GetConversation(r.Context(), r.PathValue("id"))
	respond(w, map[string]any{"conversation": c}, err)
}

func (s *Server) updateConversation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title    *string         `json:"title"`
		Archived *bool           `json:"archived"`
		Model    json.RawMessage `json:"model"`
		Effort   json.RawMessage `json:"effort"`
	}
	if !decode(w, r, 4<<10, &body, bodyRequired) {
		return
	}
	id := r.PathValue("id")
	u := store.ConversationUpdate{Title: body.Title, Archived: body.Archived}
	for _, f := range []struct {
		name string
		raw  json.RawMessage
		to   **string
	}{{"model", body.Model, &u.Model}, {"effort", body.Effort, &u.Effort}} {
		if f.raw == nil {
			continue
		}
		// null returns the field to the runner's own, as "" does.
		var v *string
		if err := json.Unmarshal(f.raw, &v); err != nil {
			writeErr(w, http.StatusBadRequest, f.name+" must be a string or null")
			return
		}
		set := ""
		if v != nil {
			set = strings.TrimSpace(*v)
		}
		*f.to = &set
	}
	if u.Model != nil || u.Effort != nil {
		c, err := s.Store.GetConversation(r.Context(), id)
		if err != nil {
			respond(w, nil, err)
			return
		}
		cfg := s.Engine.Current().Config
		a, ok := cfg.Agents[c.Agent]
		if !ok {
			writeErr(w, http.StatusConflict, "the conversation's agent "+strconv.Quote(c.Agent)+" is not configured")
			return
		}
		model, effort := deref(c.Model), deref(c.Effort)
		if u.Model != nil {
			model = *u.Model
		}
		if u.Effort != nil {
			effort = *u.Effort
		}
		if err := spec.CheckModel(cfg.Deployment.Runners[a.Runner].Kind, model, effort); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	err := s.Store.UpdateConversation(r.Context(), id, u)
	if err != nil {
		respond(w, nil, err)
		return
	}
	c, err := s.Store.GetConversation(r.Context(), id)
	respond(w, map[string]any{"conversation": c}, err)
}

func (s *Server) conversationThread(w http.ResponseWriter, r *http.Request) {
	servePage(w, r, func(before *store.ThreadCursor, limit int) (*store.ThreadPage, error) {
		return s.Store.ConversationThread(r.Context(), r.PathValue("id"), before, limit)
	})
}

func (s *Server) conversationMessage(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.GetConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	a, ok := s.Engine.Current().Config.Agents[c.Agent]
	if !ok {
		writeErr(w, http.StatusConflict, "the conversation's agent "+strconv.Quote(c.Agent)+" is not configured")
		return
	}
	s.postTo(w, r, a, c.ID)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// forkConversation serves POST /conversations/{id}/fork (docs/api.md).
func (s *Server) forkConversation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MessageID       string `json:"message_id"`
		ClientRequestID string `json:"client_request_id"`
	}
	if !decode(w, r, 4<<10, &body, bodyRequired) {
		return
	}
	if body.MessageID == "" {
		writeErr(w, http.StatusBadRequest, "message_id is required")
		return
	}
	// A retry with the same client_request_id returns the first fork, as
	// a retried create does.
	c, _, err := s.Store.ForkConversation(r.Context(), store.ForkRequest{ConversationID: r.PathValue("id"),
		MessageID: body.MessageID, ClientID: body.ClientRequestID})
	respondStatus(w, http.StatusCreated, map[string]any{"conversation": c}, err)
}
