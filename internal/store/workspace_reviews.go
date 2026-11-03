package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Review comments on a workspace (docs/api.md, Workspace, Review
// comments): drafts on a path and line range, anchored to a run's changes
// or the file as it is, edited as new versions, and sent to the
// conversation's agent as one message. Delivery is a ticket review's: a
// sent comment is delivered once a run that read its message succeeded.

// What a workspace comment is, derived from its row and the runs that read
// its message.
const (
	CommentSent = "sent"
)

// The anchors of a workspace comment.
const (
	AnchorRun      = "run"
	AnchorWorktree = "worktree"
	AnchorGit      = "git"
)

// CommentAnchor is what a comment's lines are of.
type CommentAnchor struct {
	Kind  string  `json:"kind"`
	RunID *string `json:"run_id,omitempty"`
}

// WorkspaceComment is one line comment on a workspace.
type WorkspaceComment struct {
	ID             string        `json:"id"`
	ConversationID string        `json:"conversation_id"`
	WorkspaceID    string        `json:"workspace_id"`
	Path           string        `json:"path"`
	StartLine      int           `json:"start_line"`
	EndLine        int           `json:"end_line"`
	Side           string        `json:"side"`
	Anchor         CommentAnchor `json:"anchor"`
	// Ref is the snapshot the lines are of, as a ref (snapshot backend).
	Ref         *string    `json:"ref"`
	Quote       string     `json:"quote"`
	Body        string     `json:"body"`
	Author      *string    `json:"author"`
	Supersedes  *string    `json:"supersedes"`
	WithdrawnAt *time.Time `json:"withdrawn_at"`
	MessageID   *string    `json:"message_id"`
	CreatedAt   time.Time  `json:"created_at"`
	Status      string     `json:"status"`
	// DeliveredBy is the run, succeeded, that read its message.
	DeliveredBy *string `json:"delivered_by"`
}

const wsCommentSelect = `SELECT c.id::text, c.conversation_id::text, c.workspace_id::text, c.path, c.start_line, c.end_line, c.side,
	c.anchor_kind, c.anchor_run_id::text, CASE WHEN c.snapshot_id IS NULL THEN NULL ELSE 's' || c.snapshot_id END,
	c.quote, c.body, c.author, c.supersedes::text, c.withdrawn_at, c.message_id::text, c.created_at,
	CASE WHEN c.withdrawn_at IS NOT NULL THEN 'withdrawn'
	     WHEN d.run_id IS NOT NULL THEN 'delivered'
	     WHEN EXISTS (SELECT 1 FROM workspace_review_comments n WHERE n.supersedes = c.id) THEN 'superseded'
	     WHEN c.message_id IS NULL THEN 'draft'
	     ELSE 'sent' END AS status,
	d.run_id::text AS delivered_by
	FROM workspace_review_comments c
	LEFT JOIN LATERAL (SELECT mr.run_id FROM message_reads mr JOIN runs r ON r.id = mr.run_id
	    WHERE mr.message_id = c.message_id AND r.status = 'succeeded' ORDER BY r.finished_at LIMIT 1) d ON true `

func scanWSComments(rows pgx.Rows) ([]WorkspaceComment, error) {
	defer rows.Close()
	var out []WorkspaceComment
	for rows.Next() {
		var c WorkspaceComment
		if err := rows.Scan(&c.ID, &c.ConversationID, &c.WorkspaceID, &c.Path, &c.StartLine, &c.EndLine, &c.Side,
			&c.Anchor.Kind, &c.Anchor.RunID, &c.Ref, &c.Quote, &c.Body, &c.Author, &c.Supersedes, &c.WithdrawnAt,
			&c.MessageID, &c.CreatedAt, &c.Status, &c.DeliveredBy); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func getWSComment(ctx context.Context, q querier, conversationID, id string) (*WorkspaceComment, error) {
	if !IsUUID(id) {
		return nil, ErrNotFound
	}
	rows, err := q.Query(ctx, wsCommentSelect+`WHERE c.id = $1 AND c.conversation_id = $2`, id, conversationID)
	if err != nil {
		return nil, err
	}
	list, err := scanWSComments(rows)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, ErrNotFound
	}
	return &list[0], nil
}

// GetWorkspaceComment returns one comment of a conversation.
func (s *Store) GetWorkspaceComment(ctx context.Context, conversationID, id string) (*WorkspaceComment, error) {
	return getWSComment(ctx, s.Pool, conversationID, id)
}

// NewWorkspaceComment is a comment a person writes. The caller has checked
// the anchor and read Quote from SnapshotID.
type NewWorkspaceComment struct {
	ConversationID string
	WorkspaceID    string
	Path           string
	StartLine      int
	EndLine        int
	Side           string
	Anchor         CommentAnchor
	SnapshotID     *int64
	Quote          string
	Body           string
	Author         string
}

// AddWorkspaceComment stores a draft.
func (s *Store) AddWorkspaceComment(ctx context.Context, n NewWorkspaceComment) (*WorkspaceComment, error) {
	if err := validComment(n.Path, n.StartLine, n.EndLine, n.Body); err != nil {
		return nil, err
	}
	var id string
	err := s.Pool.QueryRow(ctx, `INSERT INTO workspace_review_comments (conversation_id, workspace_id, path, start_line, end_line,
		side, anchor_kind, anchor_run_id, snapshot_id, quote, body, author)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12, '')) RETURNING id::text`,
		n.ConversationID, n.WorkspaceID, n.Path, n.StartLine, n.EndLine, n.Side, n.Anchor.Kind, n.Anchor.RunID, n.SnapshotID,
		n.Quote, n.Body, n.Author).Scan(&id)
	if err != nil {
		return nil, err
	}
	return s.GetWorkspaceComment(ctx, n.ConversationID, id)
}

// EditWorkspaceComment writes a new version of a comment, a draft that
// supersedes it, on the same path, side and anchor; the old row stays as
// it was. ErrConflict means it was withdrawn or already edited.
func (s *Store) EditWorkspaceComment(ctx context.Context, conversationID, id string, start, end int, quote, body, author string) (*WorkspaceComment, error) {
	var out *WorkspaceComment
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM workspace_review_comments WHERE id = $1 AND conversation_id = $2 FOR UPDATE`, id, conversationID); err != nil {
			return err
		}
		old, err := getWSComment(ctx, tx, conversationID, id)
		if err != nil {
			return err
		}
		if old.Status == CommentWithdrawn || old.Status == CommentSuperseded {
			return fmt.Errorf("comment %s is %s: %w", id, old.Status, ErrConflict)
		}
		if err := validComment(old.Path, start, end, body); err != nil {
			return err
		}
		var nid string
		err = tx.QueryRow(ctx, `INSERT INTO workspace_review_comments (conversation_id, workspace_id, path, start_line, end_line,
			side, anchor_kind, anchor_run_id, snapshot_id, quote, body, author, supersedes)
			SELECT conversation_id, workspace_id, path, $2, $3, side, anchor_kind, anchor_run_id, snapshot_id, $4, $5, NULLIF($6, ''), id
			FROM workspace_review_comments WHERE id = $1 RETURNING id::text`, id, start, end, quote, body, author).Scan(&nid)
		if isUniqueViolation(err) {
			return fmt.Errorf("comment %s was already edited: %w", id, ErrConflict)
		}
		if err != nil {
			return err
		}
		out, err = getWSComment(ctx, tx, conversationID, nid)
		return err
	})
	return out, err
}

// WithdrawWorkspaceComment withdraws a draft; a sent comment is edited
// instead.
func (s *Store) WithdrawWorkspaceComment(ctx context.Context, conversationID, id string) error {
	if !IsUUID(id) {
		return ErrNotFound
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE workspace_review_comments c SET withdrawn_at = now()
		WHERE c.id = $1 AND c.conversation_id = $2 AND c.message_id IS NULL AND c.withdrawn_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM workspace_review_comments n WHERE n.supersedes = c.id)`, id, conversationID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.GetWorkspaceComment(ctx, conversationID, id); err != nil {
			return err
		}
		return fmt.Errorf("comment %s is not a draft: %w", id, ErrConflict)
	}
	return nil
}

// WorkspaceComments returns every version of every comment of a
// conversation, oldest first, or those with one status.
func (s *Store) WorkspaceComments(ctx context.Context, conversationID, status string) ([]WorkspaceComment, error) {
	rows, err := s.Pool.Query(ctx, `SELECT * FROM (`+wsCommentSelect+`WHERE c.conversation_id = $1) x
		WHERE $2 = '' OR x.status = $2 ORDER BY x.created_at, x.id`, conversationID, status)
	if err != nil {
		return nil, err
	}
	return scanWSComments(rows)
}

// WorkspaceReview is a review to send.
type WorkspaceReview struct {
	ConversationID string
	WorkspaceID    string
	Agent          string
	SpecSHA        string
	// IDs names the drafts to send; empty sends every draft.
	IDs       []string
	Note      string
	Wake      bool
	By        string
	ClientID  string
	RequestID string
}

// SubmitWorkspaceReview sends drafts as one message to the conversation
// (FormatWorkspaceReview), in one transaction with each draft's message_id
// and a review event per comment; with Wake, a run is queued for the
// message as for any message. ErrInvalid means there is nothing to send or
// a named comment is not a draft of the conversation.
func (s *Store) SubmitWorkspaceReview(ctx context.Context, rv WorkspaceReview) (res PostResult, replayed bool, err error) {
	if len(rv.Note) > MaxCommentBytes {
		return res, false, fmt.Errorf("%w: a review note is at most %d bytes", ErrInvalid, MaxCommentBytes)
	}
	for _, id := range rv.IDs {
		if !IsUUID(id) {
			return res, false, fmt.Errorf("%w: comment %q is not an id", ErrInvalid, id)
		}
	}
	req := map[string]any{"op": "workspace_review", "conversation": rv.ConversationID, "by": rv.By, "note": rv.Note,
		"comments": rv.IDs, "wake": rv.Wake}
	raw, replayed, err := s.withReceipt(ctx, rv.Agent, rv.ClientID, req, func(tx pgx.Tx) (json.RawMessage, error) {
		// The drafts, locked: a second review waits, then finds them sent.
		rows, err := tx.Query(ctx, `SELECT c.id::text FROM workspace_review_comments c
			WHERE c.conversation_id = $1 AND c.message_id IS NULL AND c.withdrawn_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM workspace_review_comments n WHERE n.supersedes = c.id)
			  AND (COALESCE(cardinality($2::uuid[]), 0) = 0 OR c.id = ANY($2::uuid[]))
			ORDER BY c.created_at, c.id FOR UPDATE`, rv.ConversationID, rv.IDs)
		if err != nil {
			return nil, err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, err
		}
		if len(rv.IDs) > 0 && len(ids) != len(dedupe(rv.IDs)) {
			return nil, fmt.Errorf("%w: every comment named must be a draft of this conversation", ErrInvalid)
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("%w: no draft comments to send", ErrInvalid)
		}
		rows, err = tx.Query(ctx, wsCommentSelect+`WHERE c.id = ANY($1::uuid[]) ORDER BY c.created_at, c.id`, ids)
		if err != nil {
			return nil, err
		}
		drafts, err := scanWSComments(rows)
		if err != nil {
			return nil, err
		}
		p, err := postMessage(ctx, tx, PostMessage{Agent: rv.Agent, SpecSHA: rv.SpecSHA, Body: FormatWorkspaceReview(rv.Note, drafts),
			Author: rv.By, Wake: rv.Wake, ConversationID: rv.ConversationID})
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE workspace_review_comments SET message_id = $2 WHERE id = ANY($1::uuid[])`, ids, p.MessageID); err != nil {
			return nil, err
		}
		for _, d := range drafts {
			if _, err := tx.Exec(ctx, `INSERT INTO workspace_events (workspace_id, person, op, path, request_id)
				VALUES ($1, NULLIF($2, ''), 'review', $3, NULLIF($4, ''))`, rv.WorkspaceID, rv.By, d.Path, rv.RequestID); err != nil {
				return nil, err
			}
		}
		return json.Marshal(p)
	})
	if err != nil {
		return res, false, err
	}
	return res, replayed, json.Unmarshal(raw, &res)
}

func dedupe(ids []string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		m[strings.ToLower(id)] = true
	}
	return m
}

// FormatWorkspaceReview is a workspace review as one message: a header,
// the person's note, then each comment with its file, lines and anchor,
// the code it is on quoted ("| "), and its body quoted ("> "), every line
// prefixed, so nothing in the code or a body can pass for another comment.
func FormatWorkspaceReview(note string, comments []WorkspaceComment) string {
	var b strings.Builder
	n := "comments"
	if len(comments) == 1 {
		n = "comment"
	}
	fmt.Fprintf(&b, "Review of the workspace: %d %s.\n", len(comments), n)
	if note = strings.TrimSpace(note); note != "" {
		b.WriteString("\n" + note + "\n")
	}
	for i, c := range comments {
		where := "the whole file"
		switch {
		case c.StartLine == c.EndLine && c.StartLine > 0:
			where = fmt.Sprintf("line %d", c.StartLine)
		case c.StartLine > 0:
			where = fmt.Sprintf("lines %d-%d", c.StartLine, c.EndLine)
		}
		on := "the file as it was when the comment was written"
		if c.Anchor.Kind == AnchorRun && c.Anchor.RunID != nil {
			run := *c.Anchor.RunID
			if len(run) > 8 {
				run = run[:8]
			}
			on = "the changes of run " + run
			if c.Side == "old" {
				on += ", before the run"
			}
		}
		fmt.Fprintf(&b, "\n%d. `%s`, %s, on %s\n", i+1, strings.ReplaceAll(c.Path, "`", "'"), where, on)
		if c.Quote != "" {
			for _, l := range strings.Split(strings.TrimSuffix(c.Quote, "\n"), "\n") {
				b.WriteString("   | " + l + "\n")
			}
		}
		for _, l := range strings.Split(strings.ReplaceAll(strings.TrimSpace(c.Body), "\r\n", "\n"), "\n") {
			b.WriteString("   > " + l + "\n")
		}
	}
	return b.String()
}
