package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Ticket states (docs/tickets.md).
const (
	TicketOpen      = "open"
	TicketPlanned   = "planned"
	TicketApproved  = "approved"
	TicketBuilding  = "building"
	TicketReview    = "review"
	TicketMerging   = "merging"
	TicketMerged    = "merged"
	TicketDone      = "done"
	TicketFailed    = "failed"
	TicketAbandoned = "abandoned"
)

// ticketEdges is every transition a request may make; the lifecycle diagram
// in docs/tickets.md is this table. ProposePlan and ApprovePlan are the only
// ways into planned and approved from open and planned, because they carry
// a plan version; Transition refuses those edges.
var ticketEdges = map[string][]string{
	TicketOpen:     {TicketPlanned, TicketAbandoned},
	TicketPlanned:  {TicketPlanned, TicketOpen, TicketApproved, TicketAbandoned},
	TicketApproved: {TicketBuilding, TicketAbandoned},
	TicketBuilding: {TicketReview, TicketFailed, TicketAbandoned},
	TicketReview:   {TicketMerging, TicketFailed, TicketAbandoned},
	TicketMerging:  {TicketMerged, TicketFailed, TicketAbandoned},
	TicketMerged:   {TicketDone},
	// failed -> merged is reconciliation: GitHub says the PR merged after
	// orbit lost track of the merge it made.
	TicketFailed: {TicketApproved, TicketOpen, TicketMerged, TicketAbandoned},
}

// questionEdges is the whole lifecycle of a question.
var questionEdges = map[string][]string{
	TicketOpen: {TicketDone, TicketAbandoned},
}

// planEdges can only be taken with a plan version (ProposePlan, ApprovePlan).
var planEdges = map[[2]string]bool{
	{TicketOpen, TicketPlanned}:     true,
	{TicketPlanned, TicketPlanned}:  true,
	{TicketPlanned, TicketApproved}: true,
}

// CanTransition reports whether a ticket of kind may go from one state to
// another.
func CanTransition(kind, from, to string) bool {
	edges := ticketEdges
	if kind == "question" {
		edges = questionEdges
	}
	return slices.Contains(edges[from], to)
}

// Ticket is a ticket as the API reports it.
type Ticket struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Title        string    `json:"title"`
	Agent        string    `json:"agent"`
	Repo         *string   `json:"repo,omitempty"`
	Status       string    `json:"status"`
	Author       *string   `json:"author,omitempty"`
	ApprovedPlan *int      `json:"approved_plan,omitempty"`
	BaseSHA      *string   `json:"base_sha,omitempty"`
	Branch       *string   `json:"branch,omitempty"`
	PRNumber     *int      `json:"pr_number,omitempty"`
	HeadSHA      *string   `json:"head_sha,omitempty"`
	MergeSHA     *string   `json:"merge_sha,omitempty"`
	Error        *string   `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

const ticketCols = `id::text, kind, title, agent, repo, status, author, approved_plan, base_sha, branch,
	pr_number, head_sha, merge_sha, error, created_at, updated_at`

// NewTicket is what filing a ticket needs. Body is its first message.
type NewTicket struct {
	Kind   string
	Title  string
	Body   string
	Agent  string
	Repo   string
	Author string
}

// CreateTicket files a ticket: the row, its first event, and its body as the
// first message of its thread, in one transaction.
func (s *Store) CreateTicket(ctx context.Context, t NewTicket) (ticketID, messageID string, err error) {
	if t.Kind != "question" && t.Kind != "request" {
		return "", "", fmt.Errorf("kind must be question or request")
	}
	if (t.Kind == "request") != (t.Repo != "") {
		return "", "", fmt.Errorf("a request names a repository and a question does not")
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO tickets (kind, title, agent, repo, author)
			VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, '')) RETURNING id::text`,
			t.Kind, t.Title, t.Agent, t.Repo, t.Author).Scan(&ticketID); err != nil {
			return err
		}
		if err := ticketEvent(ctx, tx, ticketID, "", TicketOpen, t.Author, nil); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO messages (agent, direction, body, author, ticket_id)
			VALUES ($1, 'to_agent', $2, NULLIF($3, ''), $4) RETURNING id::text`,
			t.Agent, t.Body, t.Author, ticketID).Scan(&messageID)
	})
	if isForeignKeyViolation(err) {
		return "", "", ErrNotFound
	}
	return ticketID, messageID, err
}

func ticketEvent(ctx context.Context, db execer, id, from, to, by string, detail map[string]any) error {
	var d []byte
	if len(detail) > 0 {
		d, _ = json.Marshal(detail)
	}
	_, err := db.Exec(ctx, `INSERT INTO ticket_events (ticket_id, from_status, to_status, by, detail)
		VALUES ($1, NULLIF($2, ''), $3, NULLIF($4, ''), $5)`, id, from, to, by, d)
	return err
}

// GetTicket returns one ticket.
func (s *Store) GetTicket(ctx context.Context, id string) (*Ticket, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+ticketCols+` FROM tickets WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Ticket])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

// TicketFilter narrows ListTickets. Before pages backwards through
// created_at.
type TicketFilter struct {
	Status string
	Agent  string
	Before *time.Time
	Limit  int
}

// ListTickets returns tickets newest first.
func (s *Store) ListTickets(ctx context.Context, f TicketFilter) ([]Ticket, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+ticketCols+` FROM tickets
		WHERE ($1 = '' OR status = $1) AND ($2 = '' OR agent = $2)
		  AND ($3::timestamptz IS NULL OR created_at < $3)
		ORDER BY created_at DESC LIMIT $4`, f.Status, f.Agent, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Ticket])
}

// lockTicket reads a ticket's kind and status FOR UPDATE, so a transition
// decided on them cannot race another.
func lockTicket(ctx context.Context, tx pgx.Tx, id string) (kind, status string, err error) {
	err = tx.QueryRow(ctx, `SELECT kind, status FROM tickets WHERE id = $1 FOR UPDATE`, id).Scan(&kind, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return kind, status, err
}

// setStatus is the one write of a ticket's status. The guard repeats the
// status the caller locked, so it is a guarded transition even if a caller
// forgot to lock.
func setStatus(ctx context.Context, tx pgx.Tx, id, from, to, by string, u TicketUpdate, detail map[string]any) error {
	tag, err := tx.Exec(ctx, `
		UPDATE tickets SET status = $3, updated_at = now(),
		    error = CASE WHEN $3 = 'failed' THEN NULLIF($4, '') ELSE NULL END,
		    base_sha = COALESCE(NULLIF($5, ''), base_sha),
		    branch = COALESCE(NULLIF($6, ''), branch),
		    pr_number = COALESCE($7, pr_number),
		    head_sha = COALESCE(NULLIF($8, ''), head_sha),
		    merge_sha = COALESCE(NULLIF($9, ''), merge_sha)
		WHERE id = $1 AND status = $2`,
		id, from, to, u.Error, u.BaseSHA, u.Branch, u.PRNumber, u.HeadSHA, u.MergeSHA)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	if u.Error != "" {
		if detail == nil {
			detail = map[string]any{}
		}
		detail["error"] = u.Error
	}
	return ticketEvent(ctx, tx, id, from, to, by, detail)
}

// TicketUpdate is what a transition records besides the new status. Empty
// fields leave the stored value; Error is kept only on a move to failed.
type TicketUpdate struct {
	Error    string
	BaseSHA  string
	Branch   string
	PRNumber *int
	HeadSHA  string
	MergeSHA string
	// Reason is recorded on the event (an abandon's why).
	Reason string
}

// TransitionTicket moves a ticket to status to, if its kind allows that from
// its current status. It returns ErrConflict otherwise.
func (s *Store) TransitionTicket(ctx context.Context, id, to, by string, u TicketUpdate) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		kind, from, err := lockTicket(ctx, tx, id)
		if err != nil {
			return err
		}
		if !CanTransition(kind, from, to) || planEdges[[2]string{from, to}] {
			return fmt.Errorf("ticket %s: %s -> %s: %w", id, from, to, ErrConflict)
		}
		var detail map[string]any
		if u.Reason != "" {
			detail = map[string]any{"reason": u.Reason}
		}
		return setStatus(ctx, tx, id, from, to, by, u, detail)
	})
}

// SendToTicket stores a person's message in a ticket's thread. A reply to a
// planned ticket reopens it: the person is discussing the plan, not
// approving it. A finished ticket takes no more messages.
func (s *Store) SendToTicket(ctx context.Context, id, body, author string) (messageID, agent string, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		_, status, err := lockTicket(ctx, tx, id)
		if err != nil {
			return err
		}
		switch status {
		case TicketDone, TicketAbandoned, TicketMerged:
			return fmt.Errorf("ticket %s is %s: %w", id, status, ErrConflict)
		case TicketPlanned:
			if err := setStatus(ctx, tx, id, status, TicketOpen, author, TicketUpdate{}, map[string]any{"reason": "reply to the plan"}); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `SELECT agent FROM tickets WHERE id = $1`, id).Scan(&agent); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO messages (agent, direction, body, author, ticket_id)
			VALUES ($1, 'to_agent', $2, NULLIF($3, ''), $4) RETURNING id::text`,
			agent, body, author, id).Scan(&messageID)
	})
	return messageID, agent, err
}

// ProposePlan stores the next plan version for a request that is open or
// planned and moves it to planned. A newer plan supersedes an unapproved one.
func (s *Store) ProposePlan(ctx context.Context, id, runID, body string) (int, error) {
	var version int
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		kind, from, err := lockTicket(ctx, tx, id)
		if err != nil {
			return err
		}
		if kind != "request" || (from != TicketOpen && from != TicketPlanned) {
			return fmt.Errorf("ticket %s (%s, %s) cannot take a plan: %w", id, kind, from, ErrConflict)
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO ticket_plans (ticket_id, version, body, run_id)
			SELECT $1, COALESCE(max(version), 0) + 1, $2, NULLIF($3, '')::uuid FROM ticket_plans WHERE ticket_id = $1
			RETURNING version`, id, body, runID).Scan(&version); err != nil {
			return err
		}
		return setStatus(ctx, tx, id, from, TicketPlanned, "agent", TicketUpdate{}, map[string]any{"plan": version})
	})
	return version, err
}

// ApprovePlan approves one plan version: only while the ticket is planned
// and that version is the newest, so nobody approves a plan they did not
// read.
func (s *Store) ApprovePlan(ctx context.Context, id string, version int, by string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		_, from, err := lockTicket(ctx, tx, id)
		if err != nil {
			return err
		}
		var newest *int
		if err := tx.QueryRow(ctx, `SELECT max(version) FROM ticket_plans WHERE ticket_id = $1`, id).Scan(&newest); err != nil {
			return err
		}
		if from != TicketPlanned || newest == nil || *newest != version {
			return fmt.Errorf("ticket %s: plan %d is not the newest plan of a planned ticket: %w", id, version, ErrConflict)
		}
		if _, err := tx.Exec(ctx, `UPDATE tickets SET approved_plan = $2 WHERE id = $1`, id, version); err != nil {
			return err
		}
		return setStatus(ctx, tx, id, from, TicketApproved, by, TicketUpdate{}, map[string]any{"plan": version})
	})
}

// TicketPlan is one stored plan version.
type TicketPlan struct {
	Version   int       `json:"version"`
	Body      string    `json:"body"`
	RunID     *string   `json:"run_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// TicketPlans returns a ticket's plans, oldest first.
func (s *Store) TicketPlans(ctx context.Context, id string) ([]TicketPlan, error) {
	rows, err := s.Pool.Query(ctx, `SELECT version, body, run_id::text, created_at FROM ticket_plans
		WHERE ticket_id = $1 ORDER BY version`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[TicketPlan])
}

// TicketEvent is one stored transition.
type TicketEvent struct {
	ID     int64           `json:"id"`
	From   *string         `json:"from,omitempty"`
	To     string          `json:"to"`
	By     *string         `json:"by,omitempty"`
	Detail json.RawMessage `json:"detail,omitempty"`
	At     time.Time       `json:"at"`
}

// TicketEvents returns a ticket's transitions, oldest first.
func (s *Store) TicketEvents(ctx context.Context, id string) ([]TicketEvent, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, from_status, to_status, by, detail, at FROM ticket_events
		WHERE ticket_id = $1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[TicketEvent])
}

// TicketMessages returns a ticket's thread messages, oldest first.
func (s *Store) TicketMessages(ctx context.Context, id string) ([]Message, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text, agent, direction, body, reply_to::text, run_id::text, author, created_at
		FROM messages WHERE ticket_id = $1 ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Message])
}

// TicketRuns returns the runs that worked on a ticket, oldest first.
func (s *Store) TicketRuns(ctx context.Context, id string) ([]Run, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+runCols+` FROM runs WHERE ticket_id = $1 ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}
