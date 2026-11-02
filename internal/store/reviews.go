package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// BuildPurposeRevise applies a person's review comments on top of the PR's
// head, without changing the plan.
const BuildPurposeRevise = "revise"

// What a review comment is, derived from its row and the runs it was given
// to (docs/tickets.md, Review comments).
const (
	CommentDraft      = "draft"
	CommentSubmitted  = "submitted"
	CommentDelivered  = "delivered"
	CommentSuperseded = "superseded"
	CommentWithdrawn  = "withdrawn"
)

// MaxCommentBytes bounds one comment's body: a review note, not a paste of
// a file; every comment rides in a build's prompt.
const MaxCommentBytes = 8 << 10

// ReviewComment is one line comment on a ticket's PR diff.
type ReviewComment struct {
	ID          string     `json:"id"`
	TicketID    string     `json:"ticket_id"`
	Path        string     `json:"path"`
	StartLine   int        `json:"start_line"`
	EndLine     int        `json:"end_line"`
	Body        string     `json:"body"`
	Author      *string    `json:"author,omitempty"`
	HeadSHA     string     `json:"head_sha"`
	Supersedes  *string    `json:"supersedes,omitempty"`
	WithdrawnAt *time.Time `json:"withdrawn_at,omitempty"`
	MessageID   *string    `json:"message_id,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	Status      string     `json:"status"`
	// DeliveredBy is the build run, succeeded, that was given it.
	DeliveredBy *string `json:"delivered_by,omitempty"`
}

// commentSelect computes each comment's status: withdrawn, delivered (a run
// given it succeeded, or was skipped delivering an earlier attempt's push),
// superseded by an edit, a draft, or submitted and waiting for a build.
const commentSelect = `SELECT c.id::text, c.ticket_id::text, c.path, c.start_line, c.end_line, c.body, c.author, c.head_sha,
	c.supersedes::text, c.withdrawn_at, c.message_id::text, c.created_at,
	CASE WHEN c.withdrawn_at IS NOT NULL THEN 'withdrawn'
	     WHEN d.run_id IS NOT NULL THEN 'delivered'
	     WHEN EXISTS (SELECT 1 FROM ticket_review_comments n WHERE n.supersedes = c.id) THEN 'superseded'
	     WHEN c.message_id IS NULL THEN 'draft'
	     ELSE 'submitted' END,
	d.run_id::text
	FROM ticket_review_comments c
	LEFT JOIN LATERAL (SELECT bc.run_id FROM ticket_build_comments bc JOIN runs r ON r.id = bc.run_id
	    WHERE bc.comment_id = c.id AND r.status IN ('succeeded', 'skipped') ORDER BY r.finished_at LIMIT 1) d ON true `

// pendingComments is the WHERE clause of comments a build is given:
// submitted, not delivered, not superseded.
const pendingComments = `c.ticket_id = $1 AND c.message_id IS NOT NULL
	AND NOT EXISTS (SELECT 1 FROM ticket_review_comments n WHERE n.supersedes = c.id)
	AND NOT EXISTS (SELECT 1 FROM ticket_build_comments bc JOIN runs r ON r.id = bc.run_id
	    WHERE bc.comment_id = c.id AND r.status IN ('succeeded', 'skipped'))`

// NewComment is a comment a person writes on a PR's diff.
type NewComment struct {
	TicketID  string
	Path      string
	StartLine int
	EndLine   int
	Body      string
	Author    string
}

// validComment checks what a person sends: a path inside the repository,
// a line range (0 and 0 for the whole file), and a body within bounds.
func validComment(p string, start, end int, body string) error {
	clean := path.Clean(p)
	switch {
	case p == "" || clean != p || strings.HasPrefix(p, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(p, 0):
		return fmt.Errorf("%w: path %q is not a clean path inside the repository", ErrInvalid, p)
	case start < 0 || end < start || (start == 0) != (end == 0):
		return fmt.Errorf("%w: lines %d-%d: start_line <= end_line, both 0 for the whole file", ErrInvalid, start, end)
	case strings.TrimSpace(body) == "":
		return fmt.Errorf("%w: a comment has a body", ErrInvalid)
	case len(body) > MaxCommentBytes || !utf8.ValidString(body):
		return fmt.Errorf("%w: a comment body is UTF-8 of at most %d bytes", ErrInvalid, MaxCommentBytes)
	}
	return nil
}

// commentable locks a ticket and returns its PR head: comments are made on
// a request's PR while it can still change.
func commentable(ctx context.Context, tx pgx.Tx, ticketID string) (string, error) {
	_, status, err := lockTicket(ctx, tx, ticketID)
	if err != nil {
		return "", err
	}
	var head *string
	if err := tx.QueryRow(ctx, `SELECT head_sha FROM tickets WHERE id = $1`, ticketID).Scan(&head); err != nil {
		return "", err
	}
	switch status {
	case TicketBuilding, TicketReview, TicketMerging, TicketFailed:
	default:
		return "", fmt.Errorf("ticket %s is %s: %w", ticketID, status, ErrConflict)
	}
	if head == nil {
		return "", fmt.Errorf("ticket %s has no PR to comment on: %w", ticketID, ErrConflict)
	}
	return *head, nil
}

func (s *Store) getComment(ctx context.Context, db pgx.Tx, id string) (*ReviewComment, error) {
	rows, err := db.Query(ctx, commentSelect+`WHERE c.id = $1`, id)
	if err != nil {
		return nil, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[ReviewComment])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &c, err
}

// AddReviewComment stores a draft comment on the ticket's PR at its current
// head.
func (s *Store) AddReviewComment(ctx context.Context, n NewComment) (*ReviewComment, error) {
	if err := validComment(n.Path, n.StartLine, n.EndLine, n.Body); err != nil {
		return nil, err
	}
	var out *ReviewComment
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		head, err := commentable(ctx, tx, n.TicketID)
		if err != nil {
			return err
		}
		var id string
		if err := tx.QueryRow(ctx, `INSERT INTO ticket_review_comments (ticket_id, path, start_line, end_line, body, author, head_sha)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7) RETURNING id::text`,
			n.TicketID, n.Path, n.StartLine, n.EndLine, n.Body, n.Author, head).Scan(&id); err != nil {
			return err
		}
		out, err = s.getComment(ctx, tx, id)
		return err
	})
	return out, err
}

// EditReviewComment writes a new version of a comment, a draft that
// supersedes it: the old row stays as it was (a submitted one is no longer
// given to builds; a delivered one stays delivered). ErrConflict means the
// comment was withdrawn or already edited.
func (s *Store) EditReviewComment(ctx context.Context, ticketID, id string, start, end int, body, author string) (*ReviewComment, error) {
	var out *ReviewComment
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		head, err := commentable(ctx, tx, ticketID)
		if err != nil {
			return err
		}
		old, err := s.getComment(ctx, tx, id)
		if err != nil {
			return err
		}
		if old.TicketID != ticketID {
			return ErrNotFound
		}
		if old.Status == CommentWithdrawn || old.Status == CommentSuperseded {
			return fmt.Errorf("comment %s is %s: %w", id, old.Status, ErrConflict)
		}
		if err := validComment(old.Path, start, end, body); err != nil {
			return err
		}
		var nid string
		err = tx.QueryRow(ctx, `INSERT INTO ticket_review_comments (ticket_id, path, start_line, end_line, body, author, head_sha, supersedes)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, $8) RETURNING id::text`,
			ticketID, old.Path, start, end, body, author, head, id).Scan(&nid)
		if isUniqueViolation(err) {
			return fmt.Errorf("comment %s was already edited: %w", id, ErrConflict)
		}
		if err != nil {
			return err
		}
		out, err = s.getComment(ctx, tx, nid)
		return err
	})
	return out, err
}

// WithdrawReviewComment withdraws a draft; a submitted comment was already
// sent and is edited instead.
func (s *Store) WithdrawReviewComment(ctx context.Context, ticketID, id string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE ticket_review_comments c SET withdrawn_at = now()
		WHERE c.id = $1 AND c.ticket_id = $2 AND c.message_id IS NULL AND c.withdrawn_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM ticket_review_comments n WHERE n.supersedes = c.id)`, id, ticketID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ticket_review_comments WHERE id = $1 AND ticket_id = $2)`, id, ticketID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		return fmt.Errorf("comment %s is not a draft: %w", id, ErrConflict)
	}
	return nil
}

// ReviewComments returns every version of every comment on a ticket,
// oldest first.
func (s *Store) ReviewComments(ctx context.Context, ticketID string) ([]ReviewComment, error) {
	rows, err := s.Pool.Query(ctx, commentSelect+`WHERE c.ticket_id = $1 ORDER BY c.created_at, c.id`, ticketID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ReviewComment])
}

// PendingReviewComments counts the comments submitted and not yet
// delivered: what the next build of the ticket is given.
func (s *Store) PendingReviewComments(ctx context.Context, ticketID string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM ticket_review_comments c WHERE `+pendingComments, ticketID).Scan(&n)
	return n, err
}

// giveComments records, inside StartBuild's transaction, that a build run
// is given every pending comment of its ticket, and marks the messages that
// carried them read by it. It returns how many.
func giveComments(ctx context.Context, tx pgx.Tx, ticketID, runID string) (int, error) {
	tag, err := tx.Exec(ctx, `INSERT INTO ticket_build_comments (run_id, comment_id)
		SELECT $2, c.id FROM ticket_review_comments c WHERE `+pendingComments, ticketID, runID)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec(ctx, `UPDATE messages SET run_id = $1 WHERE run_id IS NULL AND id IN (
		SELECT c.message_id FROM ticket_review_comments c JOIN ticket_build_comments bc ON bc.comment_id = c.id WHERE bc.run_id = $1)`, runID)
	return int(tag.RowsAffected()), err
}

// BuildComments returns the comments a build run was given, oldest first:
// what its prompt carries.
func (s *Store) BuildComments(ctx context.Context, runID string) ([]ReviewComment, error) {
	rows, err := s.Pool.Query(ctx, commentSelect+`JOIN ticket_build_comments g ON g.comment_id = c.id
		WHERE g.run_id = $1 ORDER BY c.created_at, c.id`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ReviewComment])
}

// ReviewSubmitted is what submitting a review did.
type ReviewSubmitted struct {
	MessageID string `json:"message_id"`
	Comments  int    `json:"comments"`
}

// SubmitReview sends a ticket's draft comments to its thread as one
// message (FormatReview), in one transaction: the message, each draft's
// message_id, and for a failed ticket the move back to review, so the loop
// starts the build that applies them. A ticket must be in review or
// failed with a PR. ErrInvalid means there is nothing to send: no draft,
// and for a failed ticket no submitted comment still waiting either.
func (s *Store) SubmitReview(ctx context.Context, ticketID, by, note, clientID string) (res ReviewSubmitted, replayed bool, err error) {
	var agent string
	if err := s.Pool.QueryRow(ctx, `SELECT agent FROM tickets WHERE id = $1`, ticketID).Scan(&agent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return res, false, ErrNotFound
		}
		return res, false, err
	}
	if len(note) > MaxCommentBytes {
		return res, false, fmt.Errorf("%w: a review note is at most %d bytes", ErrInvalid, MaxCommentBytes)
	}
	req := map[string]any{"op": "review", "ticket": ticketID, "by": by, "note": note}
	raw, replayed, err := s.withReceipt(ctx, agent, clientID, req, func(tx pgx.Tx) (json.RawMessage, error) {
		_, status, err := lockTicket(ctx, tx, ticketID)
		if err != nil {
			return nil, err
		}
		var conv string
		var pr *int
		var head *string
		if err := tx.QueryRow(ctx, `SELECT conversation_id::text, pr_number, head_sha FROM tickets WHERE id = $1`, ticketID).
			Scan(&conv, &pr, &head); err != nil {
			return nil, err
		}
		if (status != TicketReview && status != TicketFailed) || pr == nil || head == nil {
			return nil, fmt.Errorf("ticket %s is %s: a review is submitted on a PR in review or failed: %w", ticketID, status, ErrConflict)
		}
		rows, err := tx.Query(ctx, commentSelect+`WHERE c.ticket_id = $1 AND c.message_id IS NULL AND c.withdrawn_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM ticket_review_comments n WHERE n.supersedes = c.id) ORDER BY c.created_at, c.id`, ticketID)
		if err != nil {
			return nil, err
		}
		drafts, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ReviewComment])
		if err != nil {
			return nil, err
		}
		var waiting int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ticket_review_comments c WHERE `+pendingComments, ticketID).Scan(&waiting); err != nil {
			return nil, err
		}
		if len(drafts) == 0 && (status != TicketFailed || waiting == 0) {
			return nil, fmt.Errorf("%w: no draft comments to send", ErrInvalid)
		}
		r := ReviewSubmitted{Comments: len(drafts)}
		if len(drafts) > 0 {
			p, err := postMessage(ctx, tx, PostMessage{Agent: agent, Body: FormatReview(*pr, *head, note, drafts), Author: by,
				ConversationID: conv})
			if err != nil {
				return nil, err
			}
			r.MessageID = p.MessageID
			ids := make([]string, 0, len(drafts))
			for _, d := range drafts {
				ids = append(ids, d.ID)
			}
			if _, err := tx.Exec(ctx, `UPDATE ticket_review_comments SET message_id = $2 WHERE id = ANY($1::uuid[])`, ids, p.MessageID); err != nil {
				return nil, err
			}
		}
		if status == TicketFailed {
			if err := setStatus(ctx, tx, ticketID, TicketFailed, TicketReview, by, TicketUpdate{},
				map[string]any{"review": r.MessageID, "comments": len(drafts) + waiting}); err != nil {
				return nil, err
			}
		}
		return json.Marshal(r)
	})
	if err != nil {
		return res, false, err
	}
	return res, replayed, json.Unmarshal(raw, &res)
}

// FormatReview is a review as one message: a header naming the PR and the
// commit the comments were written against, the person's note, then each
// comment with its file and lines and its body quoted line by line. Every
// body line is quoted, so nothing in a comment can pass for the start of
// another.
func FormatReview(pr int, head, note string, comments []ReviewComment) string {
	var b strings.Builder
	n := "comments"
	if len(comments) == 1 {
		n = "comment"
	}
	if len(head) > 12 {
		head = head[:12]
	}
	fmt.Fprintf(&b, "Review of PR #%d at %s: %d %s.\n", pr, head, len(comments), n)
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
		fmt.Fprintf(&b, "\n%d. `%s`, %s\n", i+1, strings.ReplaceAll(c.Path, "`", "'"), where)
		for _, l := range strings.Split(strings.ReplaceAll(strings.TrimSpace(c.Body), "\r\n", "\n"), "\n") {
			b.WriteString("   > " + l + "\n")
		}
	}
	return b.String()
}
