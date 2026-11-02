package api

import (
	"net/http"

	"github.com/kensac/orbit/internal/store"
)

// Review comments on a ticket's PR diff (docs/tickets.md, Review
// comments): drafts a person writes, edits as new versions, and one submit
// that sends them to the ticket's thread as one message. The ticket loop
// starts the build that applies them; a comment is delivered when that
// build succeeds. The author is the token's person; with the operator
// token, the body's author.

func (s *Server) listComments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.Store.GetTicket(r.Context(), id); err != nil {
		respond(w, nil, err)
		return
	}
	c, err := s.Store.ReviewComments(r.Context(), id)
	respond(w, map[string]any{"comments": orEmpty(c)}, err)
}

type commentBody struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Body      string `json:"body"`
	Author    string `json:"author"`
}

func (s *Server) addComment(w http.ResponseWriter, r *http.Request) {
	var b commentBody
	if !decodeBody(w, r, &b) {
		return
	}
	c, err := s.Store.AddReviewComment(r.Context(), store.NewComment{TicketID: r.PathValue("id"), Path: b.Path,
		StartLine: b.StartLine, EndLine: b.EndLine, Body: b.Body, Author: actor(r, b.Author)})
	respondStatus(w, http.StatusCreated, map[string]any{"comment": c}, err)
}

func (s *Server) editComment(w http.ResponseWriter, r *http.Request) {
	var b commentBody
	if !decodeBody(w, r, &b) {
		return
	}
	c, err := s.Store.EditReviewComment(r.Context(), r.PathValue("id"), r.PathValue("comment"), b.StartLine, b.EndLine, b.Body, actor(r, b.Author))
	respondStatus(w, http.StatusCreated, map[string]any{"comment": c}, err)
}

func (s *Server) withdrawComment(w http.ResponseWriter, r *http.Request) {
	err := s.Store.WithdrawReviewComment(r.Context(), r.PathValue("id"), r.PathValue("comment"))
	respond(w, map[string]any{"ok": err == nil}, err)
}

func (s *Server) submitReview(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Body            string `json:"body"`
		Author          string `json:"author"`
		ClientRequestID string `json:"client_request_id"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	res, _, err := s.Store.SubmitReview(r.Context(), r.PathValue("id"), actor(r, b.Author), b.Body, b.ClientRequestID)
	respondStatus(w, http.StatusCreated, res, err)
}
