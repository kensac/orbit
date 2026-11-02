package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// OrbitRerun is requested_by for the ticket loop's own reruns.
const OrbitRerun = "orbit"

// Rerun is one rerun of a PR's failed checks.
type Rerun struct {
	ID          int64           `json:"id"`
	TicketID    string          `json:"ticket_id"`
	HeadSHA     string          `json:"head_sha"`
	RequestedBy string          `json:"requested_by"`
	RequestedAt time.Time       `json:"requested_at"`
	DoneAt      *time.Time      `json:"done_at,omitempty"`
	Jobs        json.RawMessage `json:"jobs,omitempty"`
	Error       *string         `json:"error,omitempty"`
}

const rerunCols = `id, ticket_id::text, head_sha, requested_by, requested_at, done_at, jobs, error`

// RequestRerun records a person's request to rerun the failed checks of a
// ticket's PR, for the ticket loop to carry out. The ticket must have a PR
// and be in review or failed. ErrConflict means it cannot, or a request
// already waits.
func (s *Store) RequestRerun(ctx context.Context, ticketID, by string) (*Rerun, error) {
	var r *Rerun
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		_, status, err := lockTicket(ctx, tx, ticketID)
		if err != nil {
			return err
		}
		var head *string
		var pr *int
		if err := tx.QueryRow(ctx, `SELECT head_sha, pr_number FROM tickets WHERE id = $1`, ticketID).Scan(&head, &pr); err != nil {
			return err
		}
		if (status != TicketReview && status != TicketFailed) || head == nil || pr == nil {
			return fmt.Errorf("ticket %s is %s: checks are rerun on a PR in review or failed: %w", ticketID, status, ErrConflict)
		}
		rows, err := tx.Query(ctx, `INSERT INTO ticket_check_reruns (ticket_id, head_sha, requested_by)
			VALUES ($1, $2, $3) RETURNING `+rerunCols, ticketID, *head, by)
		if err != nil {
			return err
		}
		got, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Rerun])
		if isUniqueViolation(err) {
			return fmt.Errorf("ticket %s: a rerun already waits: %w", ticketID, ErrConflict)
		}
		r = &got
		return err
	})
	return r, err
}

// PendingRerun returns the rerun a person asked for that the loop has not
// carried out, or nil.
func (s *Store) PendingRerun(ctx context.Context, ticketID string) (*Rerun, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+rerunCols+` FROM ticket_check_reruns WHERE ticket_id = $1 AND done_at IS NULL`, ticketID)
	if err != nil {
		return nil, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Rerun])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

// OrbitReruns counts the loop's own reruns of a ticket's checks at head:
// what it has spent on that commit before a fix.
func (s *Store) OrbitReruns(ctx context.Context, ticketID, head string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM ticket_check_reruns
		WHERE ticket_id = $1 AND head_sha = $2 AND requested_by = $3`, ticketID, head, OrbitRerun).Scan(&n)
	return n, err
}

// RecordRerun stores a rerun the loop made on its own, done: jobs it
// reran, or why it could not (errMsg).
func (s *Store) RecordRerun(ctx context.Context, ticketID, head string, jobs json.RawMessage, errMsg string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO ticket_check_reruns (ticket_id, head_sha, requested_by, done_at, jobs, error)
		VALUES ($1, $2, $3, now(), $4, NULLIF($5, ''))`, ticketID, head, OrbitRerun, nullJSON(jobs), errMsg)
	return err
}

// FinishRerun records the outcome of a person's rerun and, when reopen is
// set, moves a failed ticket back to review in the same transaction, so
// the loop watches the rerun checks: only while its head is still the one
// the request named. A request already finished is ErrConflict.
func (s *Store) FinishRerun(ctx context.Context, id int64, jobs json.RawMessage, errMsg string, reopen bool) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// The ticket is locked first, as RequestRerun locks it, so the two
		// never wait on each other in opposite orders.
		var ticketID string
		err := tx.QueryRow(ctx, `SELECT ticket_id::text FROM ticket_check_reruns WHERE id = $1`, id).Scan(&ticketID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, status, err := lockTicket(ctx, tx, ticketID)
		if err != nil {
			return err
		}
		var head, by string
		err = tx.QueryRow(ctx, `UPDATE ticket_check_reruns SET done_at = now(), jobs = $2, error = NULLIF($3, '')
			WHERE id = $1 AND done_at IS NULL RETURNING head_sha, requested_by`, id, nullJSON(jobs), errMsg).Scan(&head, &by)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("rerun %d: %w (already done)", id, ErrConflict)
		}
		if err != nil || !reopen || errMsg != "" || status != TicketFailed {
			return err
		}
		var cur *string
		if err := tx.QueryRow(ctx, `SELECT head_sha FROM tickets WHERE id = $1`, ticketID).Scan(&cur); err != nil {
			return err
		}
		if cur == nil || *cur != head {
			return nil
		}
		return setStatus(ctx, tx, ticketID, TicketFailed, TicketReview, by, TicketUpdate{}, map[string]any{"rerun": id})
	})
}

// TicketReruns returns every rerun of a ticket's checks, oldest first.
func (s *Store) TicketReruns(ctx context.Context, ticketID string) ([]Rerun, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+rerunCols+` FROM ticket_check_reruns WHERE ticket_id = $1 ORDER BY id`, ticketID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Rerun])
}
