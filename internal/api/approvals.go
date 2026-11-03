package api

import (
	"net/http"
	"slices"

	"github.com/kensac/orbit/internal/store"
)

// Approvals (docs/api.md, Approvals): a chat run's permission requests waiting for its person. Deciding is one guarded update; the waiting run reads it within a second, on whichever instance holds the CLI.

// maxApprovalMessage bounds what a person tells the model with a decision.
const maxApprovalMessage = 2000

var approvalStates = []string{store.ApprovalPending, store.ApprovalAllowed, store.ApprovalDenied, store.ApprovalExpired, store.ApprovalWithdrawn}

func (s *Server) getApproval(w http.ResponseWriter, r *http.Request) {
	a, err := s.Store.GetApproval(r.Context(), r.PathValue("id"))
	respond(w, map[string]any{"approval": a}, err)
}

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.GetConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	state := r.URL.Query().Get("state")
	if state != "" && !slices.Contains(approvalStates, state) {
		writeErr(w, http.StatusBadRequest, "state must be one of pending, allowed, denied, expired, withdrawn")
		return
	}
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	list, err := s.Store.ListApprovals(r.Context(), c.ID, state, limit)
	respond(w, map[string]any{"approvals": list}, err)
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allow   *bool  `json:"allow"`
		Scope   string `json:"scope"`
		Message string `json:"message"`
		By      string `json:"by"`
	}
	if !decode(w, r, 8<<10, &body, bodyRequired) {
		return
	}
	if body.Allow == nil {
		writeErr(w, http.StatusBadRequest, "allow is required")
		return
	}
	switch body.Scope {
	case "":
		body.Scope = store.ScopeOnce
	case store.ScopeOnce, store.ScopeConversation:
	default:
		writeErr(w, http.StatusBadRequest, "scope must be once or conversation")
		return
	}
	if len(body.Message) > maxApprovalMessage {
		writeErr(w, http.StatusBadRequest, "message is at most 2000 bytes")
		return
	}
	// A person decides as themselves; by is for the operator token.
	by := body.By
	if p := principalOf(r); p.Person != "" {
		by = p.Person
	}
	if by == "" {
		by = "operator"
	}
	a, err := s.Store.DecideApproval(r.Context(), r.PathValue("id"), *body.Allow, body.Scope, body.Message, by)
	respond(w, map[string]any{"approval": a}, err)
}
