package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// A conversation's permission level (docs/api.md, Conversation permissions): versioned rows, the newest unexpired one in force for the runs its person's messages start, never wider than the agent's ceiling. A running reply takes a change within a poll (engine.levelPoll).

// MaxPermissionTTL bounds expires_in: a raised level is for a session of work, not a standing grant (that is the agent's spec).
const MaxPermissionTTL = 7 * 24 * time.Hour

// permissionHistory is how many versions GET returns.
const permissionHistory = 50

// permissionsView is a conversation's permissions as the API shows them.
type permissionsView struct {
	// Level is what the next run its person's message starts runs at.
	Level string `json:"level"`
	// Source is "conversation" when its own version sets Level, else "agent".
	Source string `json:"source"`
	// AgentLevel is the agent's own level, what unattended runs start from.
	AgentLevel string `json:"agent_level"`
	Ceiling    string `json:"ceiling"`
	// Levels are the levels a PUT may set, narrowest first.
	Levels []string `json:"levels"`
	// Current is the newest version, in force or not; null for none.
	Current *store.ConversationPermission `json:"current"`
	// History is every version, newest first: GET only.
	History *[]store.ConversationPermission `json:"history,omitempty"`
}

func viewPermissions(a *spec.Agent, cp *store.ConversationPermission, now time.Time) permissionsView {
	v := permissionsView{Level: a.PermissionLevel(), Source: "agent", AgentLevel: a.PermissionLevel(), Ceiling: a.PermissionCeiling(), Current: cp}
	for _, l := range spec.Levels {
		if spec.LevelRank(l) <= spec.LevelRank(v.Ceiling) {
			v.Levels = append(v.Levels, l)
		}
	}
	if cp.InForce(now) {
		v.Level, v.Source = a.ResolveLevel(cp.Level), "conversation"
	}
	return v
}

// conversationAgent is a conversation and its configured agent, or the error already written.
func (s *Server) conversationAgent(w http.ResponseWriter, r *http.Request) (*store.Conversation, *spec.Agent, bool) {
	c, err := s.Store.GetConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return nil, nil, false
	}
	a, ok := s.Engine.Current().Config.Agents[c.Agent]
	if !ok {
		writeErr(w, http.StatusConflict, "the conversation's agent "+strconv.Quote(c.Agent)+" is not configured")
		return nil, nil, false
	}
	return c, a, true
}

func (s *Server) getPermissions(w http.ResponseWriter, r *http.Request) {
	c, a, ok := s.conversationAgent(w, r)
	if !ok {
		return
	}
	hist, err := s.Store.ConversationPermissions(r.Context(), c.ID, permissionHistory)
	if err != nil {
		respond(w, nil, err)
		return
	}
	var cur *store.ConversationPermission
	if len(hist) > 0 {
		cur = &hist[0]
	}
	v := viewPermissions(a, cur, time.Now())
	v.History = &hist
	respond(w, map[string]any{"permissions": v}, nil)
}

func (s *Server) putPermissions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Level     json.RawMessage `json:"level"`
		ExpiresIn string          `json:"expires_in"`
		By        string          `json:"by"`
	}
	if !decode(w, r, 4<<10, &body, bodyRequired) {
		return
	}
	c, a, ok := s.conversationAgent(w, r)
	if !ok {
		return
	}
	level := ""
	if len(body.Level) == 0 {
		writeErr(w, http.StatusBadRequest, "level is required (null returns the conversation to its agent's own)")
		return
	}
	if string(body.Level) != "null" {
		if json.Unmarshal(body.Level, &level) != nil || spec.LevelRank(level) < 0 {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("level must be one of %v, or null", spec.Levels))
			return
		}
		if ceiling := a.PermissionCeiling(); spec.LevelRank(level) > spec.LevelRank(ceiling) {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s is above the agent's ceiling, %s", level, ceiling))
			return
		}
	}
	var expires *time.Time
	if body.ExpiresIn != "" {
		d, err := time.ParseDuration(body.ExpiresIn)
		if err != nil || d <= 0 || d > MaxPermissionTTL {
			writeErr(w, http.StatusBadRequest, "expires_in must be a duration such as 1h, at most "+MaxPermissionTTL.String())
			return
		}
		if level == "" {
			writeErr(w, http.StatusBadRequest, "expires_in needs a level: the agent's own does not expire")
			return
		}
		t := time.Now().Add(d).UTC().Truncate(time.Second)
		expires = &t
	}
	// A person sets it as themselves; by is for the operator token.
	by := body.By
	if p := principalOf(r); p.Person != "" {
		by = p.Person
	}
	if by == "" {
		by = "operator"
	}
	cp, err := s.Store.SetConversationPermission(r.Context(), c.ID, level, by, expires)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"permissions": viewPermissions(a, &cp, time.Now())}, nil)
}
