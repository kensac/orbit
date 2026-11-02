package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/kensac/orbit/internal/store"
)

// Conversations: an agent's threads, as many as a person opens. The agent's
// default conversation is also /agents/{name}/thread and /messages.

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	var before *store.ConversationCursor
	if b := r.URL.Query().Get("before"); b != "" {
		c, err := store.ParseConversationCursor(b)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "before: "+err.Error())
			return
		}
		before = c
	}
	archived := r.URL.Query().Get("archived") == "true"
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	page, err := s.Store.ListConversations(r.Context(), r.PathValue("name"), archived, before, limit)
	if err != nil {
		respond(w, nil, err)
		return
	}
	resp := map[string]any{"conversations": orEmpty(page.Conversations), "has_more": page.HasMore, "next_before": nil}
	if page.HasMore {
		resp["next_before"] = page.NextBefore
	}
	respond(w, resp, nil)
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
	}
	if !decode(w, r, 64<<10, &body, bodyOptional) {
		return
	}
	a, ok := s.Engine.Current().Config.Agents[r.PathValue("name")]
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
		Title    *string `json:"title"`
		Archived *bool   `json:"archived"`
	}
	if !decode(w, r, 4<<10, &body, bodyRequired) {
		return
	}
	id := r.PathValue("id")
	err := s.Store.UpdateConversation(r.Context(), id, store.ConversationUpdate{Title: body.Title, Archived: body.Archived})
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
