package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/kensac/orbit/internal/engine"
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
		// FromSession starts the conversation from an imported session (its orbit id or its source's session id): its first runs resume that session.
		FromSession string `json:"from_session"`
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
	var seed *store.SessionSeed
	if body.FromSession != "" {
		cfg := s.Engine.Current().Config
		if cfg.Deployment.Runners[a.Runner].Kind != "claude-code" || a.Session == nil || !a.Session.Resume {
			respond(w, nil, fmt.Errorf("%w: agent %s does not resume Claude Code sessions (it needs a claude-code runner and session.resume)", store.ErrInvalid, a.Name))
			return
		}
		var err error
		if seed, _, err = s.Engine.SessionSeed(r.Context(), body.FromSession); err != nil {
			if errors.Is(err, engine.ErrSeedTooLarge) {
				err = fmt.Errorf("%w: %w", store.ErrInvalid, err)
			}
			respond(w, nil, err)
			return
		}
	}
	res, replayed, err := s.Store.CreateConversation(r.Context(), store.NewConversation{
		Agent: a.Name, SpecSHA: a.SHA, Title: body.Title, Body: strings.TrimSpace(body.Body),
		Author: actor(r, body.Author), Wake: wake, ClientID: body.ClientRequestID, Seed: seed,
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
