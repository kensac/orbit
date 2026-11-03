package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/kensac/orbit/internal/diff"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/workspace"
)

// Review comments on a workspace (docs/api.md, Workspace, Review
// comments): drafts on a path and lines of a run's changes or the file as
// it is, edits as new versions, and one review that sends them to the
// conversation's agent as one message, formatted by code. The code a
// comment is on is quoted from the snapshot its lines are of when it is
// written, so the agent reads what the person saw.

// Quote limits: a comment quotes a few lines, not a file.
const (
	quoteLines = 40
	quoteBytes = 4 << 10
)

func (s *Server) workspaceReviewRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/v1/conversations/{id}/comments", s.listWorkspaceComments)
	api("POST /api/v1/conversations/{id}/comments", s.addWorkspaceComment)
	api("POST /api/v1/conversations/{id}/comments/{comment}/edit", s.editWorkspaceComment)
	api("POST /api/v1/conversations/{id}/comments/{comment}/withdraw", s.withdrawWorkspaceComment)
	api("POST /api/v1/conversations/{id}/review", s.submitWorkspaceReview)
}

// reviewTarget is the conversation of a comment route and its workspace.
func (s *Server) reviewTarget(w http.ResponseWriter, r *http.Request) (*store.Conversation, *store.WorkspaceTree, bool) {
	c, err := s.Store.GetConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		wsFail(w, err)
		return nil, nil, false
	}
	if c.WorkspaceID == nil && !s.chatAgent(c.Agent) {
		wsErr(w, http.StatusNotFound, "no_workspace", "the conversation has no workspace")
		return nil, nil, false
	}
	t, err := s.Store.ConversationWorkspace(r.Context(), c.ID)
	if err != nil {
		wsFail(w, err)
		return nil, nil, false
	}
	return c, t, true
}

// commentFail answers a comment store error with its check.
func commentFail(w http.ResponseWriter, err error) {
	var we *wsError
	switch {
	case errors.As(err, &we):
		wsErrWith(w, we.status, we.check, we.msg, we.extra)
	case errors.Is(err, store.ErrInvalid):
		wsErr(w, http.StatusBadRequest, "request", err.Error())
	case errors.Is(err, store.ErrConflict):
		wsErr(w, http.StatusConflict, "state", err.Error())
	case errors.Is(err, store.ErrReceiptMismatch):
		wsErr(w, http.StatusConflict, "request", err.Error())
	default:
		wsFail(w, err)
	}
}

// quote is the lines start..end of a file in a snapshot, for a comment on
// them: "" for the whole file. The file must be there, text, and long
// enough.
func quote(snap *workspace.Snapshot, p string, start, end int) (string, error) {
	e, err := snap.Lookup(p)
	if err != nil {
		if errors.Is(err, workspace.ErrPath) {
			return "", wsErrorf(http.StatusBadRequest, "path", "%s", err.Error())
		}
		return "", wsErrorf(http.StatusNotFound, "not_found", "%s is not in the file the comment is on", p)
	}
	if e.Kind != workspace.KindFile {
		return "", wsErrorf(http.StatusBadRequest, "path", "path: %q is not a file", p)
	}
	if start == 0 {
		return "", nil
	}
	if diff.IsBinary(e.Data()) {
		return "", wsErrorf(http.StatusBadRequest, "request", "%s is binary: comment on the whole file (lines 0 and 0)", p)
	}
	lines := diff.SplitLines(e.Data())
	if end > len(lines) {
		return "", wsErrorf(http.StatusBadRequest, "request", "lines %d-%d: %s has %d lines", start, end, p, len(lines))
	}
	var b strings.Builder
	for i, l := range lines[start-1 : end] {
		if i == quoteLines || b.Len()+len(l) > quoteBytes {
			fmt.Fprintf(&b, "... (%d more lines)\n", end-start+1-i)
			break
		}
		b.WriteString(l + "\n")
	}
	return b.String(), nil
}

// anchorBody is a comment's anchor as sent.
type anchorBody struct {
	Kind  string `json:"kind"`
	RunID string `json:"run_id"`
	Repo  string `json:"repo"`
	Base  string `json:"base"`
	Head  string `json:"head"`
}

// anchorSnapshot resolves an anchor and side to the snapshot whose lines a
// comment is on.
func (s *Server) anchorSnapshot(ctx context.Context, c *store.Conversation, a anchorBody, side string) (*store.Workspace, store.CommentAnchor, error) {
	switch a.Kind {
	case store.AnchorRun:
		rs, err := s.Store.GetRunSnapshots(ctx, a.RunID)
		if errors.Is(err, store.ErrNotFound) || err == nil && (rs.ConversationID == nil || *rs.ConversationID != c.ID) {
			return nil, store.CommentAnchor{}, wsErrorf(http.StatusNotFound, "not_found", "anchor: no run %q in this conversation", a.RunID)
		}
		if err != nil {
			return nil, store.CommentAnchor{}, err
		}
		snap := rs.After
		if side == "old" {
			snap = rs.Before
		}
		id := rs.RunID
		return snap, store.CommentAnchor{Kind: store.AnchorRun, RunID: &id}, nil
	case store.AnchorWorktree:
		if side != "new" {
			return nil, store.CommentAnchor{}, wsErrorf(http.StatusBadRequest, "request", "side: the worktree has only the new side")
		}
		meta, err := s.Store.LatestSnapshot(ctx, c.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, store.CommentAnchor{Kind: store.AnchorWorktree}, nil
		}
		return meta, store.CommentAnchor{Kind: store.AnchorWorktree}, err
	case store.AnchorGit:
		return nil, store.CommentAnchor{}, wsErrorf(http.StatusConflict, "no_git", "this workspace has no git: anchor a comment to a run or the worktree")
	}
	return nil, store.CommentAnchor{}, wsErrorf(http.StatusBadRequest, "request", "anchor.kind: %q is not run, git or worktree", a.Kind)
}

// quoteAt quotes lines of a file in a stored snapshot (nil: an empty tree).
func (s *Server) quoteAt(ctx context.Context, meta *store.Workspace, p string, start, end int) (string, error) {
	snap := emptySnapshot
	if meta != nil {
		var err error
		if snap, err = s.snapshotByID(ctx, meta.ID); err != nil {
			return "", err
		}
	}
	return quote(snap, p, start, end)
}

func (s *Server) addWorkspaceComment(w http.ResponseWriter, r *http.Request) {
	c, t, ok := s.reviewTarget(w, r)
	if !ok {
		return
	}
	var b struct {
		Path      string     `json:"path"`
		StartLine int        `json:"start_line"`
		EndLine   int        `json:"end_line"`
		Side      string     `json:"side"`
		Anchor    anchorBody `json:"anchor"`
		Body      string     `json:"body"`
		Author    string     `json:"author"`
	}
	if !decode(w, r, 64<<10, &b, bodyRequired) {
		return
	}
	p, err := workspace.CleanPath(b.Path)
	if err == nil && p == "" {
		err = errors.New("a comment is on a file, not the root")
	}
	if err != nil {
		wsErr(w, http.StatusBadRequest, "path", "path: "+strings.TrimPrefix(err.Error(), "path: "))
		return
	}
	if b.Side == "" {
		b.Side = "new"
	}
	if b.Side != "new" && b.Side != "old" {
		wsErr(w, http.StatusBadRequest, "request", "side: "+strconv.Quote(b.Side)+" is not new or old")
		return
	}
	if b.StartLine < 0 || b.EndLine < b.StartLine || (b.StartLine == 0) != (b.EndLine == 0) {
		wsErr(w, http.StatusBadRequest, "request", fmt.Sprintf("lines %d-%d: start_line <= end_line, both 0 for the whole file", b.StartLine, b.EndLine))
		return
	}
	meta, anchor, err := s.anchorSnapshot(r.Context(), c, b.Anchor, b.Side)
	if err != nil {
		commentFail(w, err)
		return
	}
	q, err := s.quoteAt(r.Context(), meta, p, b.StartLine, b.EndLine)
	if err != nil {
		commentFail(w, err)
		return
	}
	n := store.NewWorkspaceComment{ConversationID: c.ID, WorkspaceID: t.ID, Path: p, StartLine: b.StartLine, EndLine: b.EndLine,
		Side: b.Side, Anchor: anchor, Quote: q, Body: b.Body, Author: actor(r, b.Author)}
	if meta != nil {
		n.SnapshotID = &meta.ID
	}
	cm, err := s.Store.AddWorkspaceComment(r.Context(), n)
	if err != nil {
		commentFail(w, err)
		return
	}
	respondStatus(w, http.StatusCreated, map[string]any{"comment": cm}, nil)
}

// commentStatuses are what a list may be narrowed to.
var commentStatuses = map[string]bool{"": true, store.CommentDraft: true, store.CommentSent: true, store.CommentDelivered: true,
	store.CommentSuperseded: true, store.CommentWithdrawn: true}

func (s *Server) listWorkspaceComments(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.reviewTarget(w, r)
	if !ok {
		return
	}
	status := r.URL.Query().Get("status")
	if !commentStatuses[status] {
		wsErr(w, http.StatusBadRequest, "request", "status: "+strconv.Quote(status)+" is not draft, sent, delivered, superseded or withdrawn")
		return
	}
	list, err := s.Store.WorkspaceComments(r.Context(), c.ID, status)
	if err != nil {
		commentFail(w, err)
		return
	}
	respond(w, map[string]any{"comments": orEmpty(list)}, nil)
}

func (s *Server) editWorkspaceComment(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.reviewTarget(w, r)
	if !ok {
		return
	}
	var b struct {
		Body      string `json:"body"`
		StartLine *int   `json:"start_line"`
		EndLine   *int   `json:"end_line"`
		Author    string `json:"author"`
	}
	if !decode(w, r, 64<<10, &b, bodyRequired) {
		return
	}
	old, err := s.Store.GetWorkspaceComment(r.Context(), c.ID, r.PathValue("comment"))
	if err != nil {
		commentFail(w, err)
		return
	}
	start, end := old.StartLine, old.EndLine
	if b.StartLine != nil {
		start = *b.StartLine
	}
	if b.EndLine != nil {
		end = *b.EndLine
	}
	if start < 0 || end < start || (start == 0) != (end == 0) {
		wsErr(w, http.StatusBadRequest, "request", fmt.Sprintf("lines %d-%d: start_line <= end_line, both 0 for the whole file", start, end))
		return
	}
	var meta *store.Workspace
	if old.Ref != nil {
		id, _ := parseSnapshotRef(*old.Ref)
		if meta, err = s.Store.GetSnapshot(r.Context(), id); err != nil {
			commentFail(w, err)
			return
		}
	}
	q, err := s.quoteAt(r.Context(), meta, old.Path, start, end)
	if err != nil {
		commentFail(w, err)
		return
	}
	cm, err := s.Store.EditWorkspaceComment(r.Context(), c.ID, old.ID, start, end, q, b.Body, actor(r, b.Author))
	if err != nil {
		commentFail(w, err)
		return
	}
	respondStatus(w, http.StatusCreated, map[string]any{"comment": cm}, nil)
}

func (s *Server) withdrawWorkspaceComment(w http.ResponseWriter, r *http.Request) {
	c, _, ok := s.reviewTarget(w, r)
	if !ok {
		return
	}
	if err := s.Store.WithdrawWorkspaceComment(r.Context(), c.ID, r.PathValue("comment")); err != nil {
		commentFail(w, err)
		return
	}
	respond(w, map[string]any{"ok": true}, nil)
}

func (s *Server) submitWorkspaceReview(w http.ResponseWriter, r *http.Request) {
	c, t, ok := s.reviewTarget(w, r)
	if !ok {
		return
	}
	var b struct {
		Note            string   `json:"note"`
		Comments        []string `json:"comments"`
		Wake            *bool    `json:"wake"`
		ClientRequestID string   `json:"client_request_id"`
		Author          string   `json:"author"`
	}
	if !decode(w, r, 64<<10, &b, bodyOptional) {
		return
	}
	a, ok := s.Engine.Current().Config.Agents[c.Agent]
	if !ok {
		writeErr(w, http.StatusConflict, "the conversation's agent "+strconv.Quote(c.Agent)+" is not configured")
		return
	}
	wake := b.Wake == nil || *b.Wake
	res, replayed, err := s.Store.SubmitWorkspaceReview(r.Context(), store.WorkspaceReview{ConversationID: c.ID, WorkspaceID: t.ID,
		Agent: a.Name, SpecSHA: a.SHA, IDs: b.Comments, Note: b.Note, Wake: wake, By: actor(r, b.Author),
		ClientID: b.ClientRequestID, RequestID: requestID(w, r)})
	if err != nil {
		commentFail(w, err)
		return
	}
	if res.RunID != "" && !replayed {
		s.Wake()
	}
	resp := map[string]any{"message_id": res.MessageID}
	if res.RunID != "" {
		resp["run_id"] = res.RunID
	}
	respondStatus(w, http.StatusCreated, resp, nil)
}
