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
	// review -> building is a fix attempt on failing checks; review ->
	// merged is reconciliation of a PR a person merged on GitHub.
	TicketReview:  {TicketMerging, TicketBuilding, TicketMerged, TicketFailed, TicketAbandoned},
	TicketMerging: {TicketMerged, TicketFailed, TicketAbandoned},
	TicketMerged:  {TicketDone},
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
	ID             string    `json:"id"`
	Kind           string    `json:"kind"`
	Title          string    `json:"title"`
	Agent          string    `json:"agent"`
	ConversationID string    `json:"conversation_id"`
	Repo           *string   `json:"repo,omitempty"`
	Status         string    `json:"status"`
	Author         *string   `json:"author,omitempty"`
	ApprovedPlan   *int      `json:"approved_plan,omitempty"`
	BaseSHA        *string   `json:"base_sha,omitempty"`
	Branch         *string   `json:"branch,omitempty"`
	PRNumber       *int      `json:"pr_number,omitempty"`
	HeadSHA        *string   `json:"head_sha,omitempty"`
	MergeSHA       *string   `json:"merge_sha,omitempty"`
	Error          *string   `json:"error,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	// MergeActionID is the github:merge action proposed for its PR.
	MergeActionID *string `json:"merge_action_id,omitempty"`
}

const ticketCols = `id::text, kind, title, agent, conversation_id::text, repo, status, author, approved_plan, base_sha, branch,
	pr_number, head_sha, merge_sha, error, created_at, updated_at, merge_action_id::text`

// NewTicket is what filing a ticket needs. Body is its first message; Wake
// enqueues a run in the ticket's conversation in the same transaction.
type NewTicket struct {
	Kind    string
	Title   string
	Body    string
	Agent   string
	SpecSHA string
	Repo    string
	Author  string
	Wake    bool
	// ClientID makes filing idempotent (receipts).
	ClientID string
}

// TicketCreated is what filing a ticket did.
type TicketCreated struct {
	TicketID       string `json:"ticket_id"`
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id"`
	RunID          string `json:"run_id,omitempty"`
}

// CreateTicket files a ticket: its conversation (titled as the ticket), the
// ticket, its first event, its body as the conversation's first message and
// the run it wakes, in one transaction, so a retry with the same ClientID
// files nothing twice. replayed reports a retry.
func (s *Store) CreateTicket(ctx context.Context, t NewTicket) (res TicketCreated, replayed bool, err error) {
	if t.Kind != "question" && t.Kind != "request" {
		return res, false, fmt.Errorf("%w: kind must be question or request", ErrInvalid)
	}
	if (t.Kind == "request") != (t.Repo != "") {
		return res, false, fmt.Errorf("%w: a request names a repository and a question does not", ErrInvalid)
	}
	req := map[string]any{"op": "ticket", "kind": t.Kind, "title": t.Title, "body": t.Body, "repo": t.Repo,
		"author": t.Author, "wake": t.Wake}
	raw, replayed, err := s.withReceipt(ctx, t.Agent, t.ClientID, req, func(tx pgx.Tx) (json.RawMessage, error) {
		var r TicketCreated
		err := tx.QueryRow(ctx, `INSERT INTO conversations (agent, title) VALUES ($1, $2) RETURNING id::text`,
			t.Agent, t.Title).Scan(&r.ConversationID)
		if isForeignKeyViolation(err) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO tickets (kind, title, agent, conversation_id, repo, author)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, '')) RETURNING id::text`,
			t.Kind, t.Title, t.Agent, r.ConversationID, t.Repo, t.Author).Scan(&r.TicketID); err != nil {
			return nil, err
		}
		if err := ticketEvent(ctx, tx, r.TicketID, "", TicketOpen, t.Author, nil); err != nil {
			return nil, err
		}
		p, err := postMessage(ctx, tx, PostMessage{Agent: t.Agent, SpecSHA: t.SpecSHA, Body: t.Body, Author: t.Author,
			Wake: t.Wake, ConversationID: r.ConversationID})
		if err != nil {
			return nil, err
		}
		r.MessageID, r.RunID = p.MessageID, p.RunID
		return json.Marshal(r)
	})
	if err != nil {
		return res, false, err
	}
	return res, replayed, json.Unmarshal(raw, &res)
}

// ticketOnMessage is what a person's message in conversation conv does to
// the ticket it belongs to, if any, inside the message's transaction: a
// reply to a planned ticket reopens it (the person is discussing the plan,
// not approving it), and a finished ticket takes no more messages. Every
// message path calls it, so no route can disagree.
func ticketOnMessage(ctx context.Context, tx pgx.Tx, conv, author string) error {
	var id, status string
	err := tx.QueryRow(ctx, `SELECT id::text, status FROM tickets WHERE conversation_id = $1 FOR UPDATE`, conv).Scan(&id, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	switch status {
	case TicketDone, TicketAbandoned, TicketMerged:
		return fmt.Errorf("ticket %s is %s: %w", id, status, ErrConflict)
	case TicketPlanned:
		return setStatus(ctx, tx, id, status, TicketOpen, author, TicketUpdate{}, map[string]any{"reason": "reply to the plan"})
	}
	return nil
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

// ProposePlan stores the next plan version for a request that is open or
// planned and moves it to planned. A newer plan supersedes an unapproved one.
func (s *Store) ProposePlan(ctx context.Context, id, runID, body string) (int, error) {
	var version int
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		version, err = proposePlan(ctx, tx, id, runID, body)
		return err
	})
	return version, err
}

// proposePlan is ProposePlan inside a caller's transaction (RecordOutcome
// stores a run's plan with the rest of its outcome). ErrConflict, before
// anything is written, means the ticket cannot take a plan now.
func proposePlan(ctx context.Context, tx pgx.Tx, id, runID, body string) (int, error) {
	kind, from, err := lockTicket(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	if kind != "request" || (from != TicketOpen && from != TicketPlanned) {
		return 0, fmt.Errorf("ticket %s (%s, %s) cannot take a plan: %w", id, kind, from, ErrConflict)
	}
	var version int
	if err := tx.QueryRow(ctx, `
		INSERT INTO ticket_plans (ticket_id, version, body, run_id)
		SELECT $1, COALESCE(max(version), 0) + 1, $2, NULLIF($3, '')::uuid FROM ticket_plans WHERE ticket_id = $1
		RETURNING version`, id, body, runID).Scan(&version); err != nil {
		return 0, err
	}
	return version, setStatus(ctx, tx, id, from, TicketPlanned, "agent", TicketUpdate{}, map[string]any{"plan": version})
}

// TicketForConversation returns the ticket discussed in a conversation, or
// nil when the conversation is not a ticket's.
func (s *Store) TicketForConversation(ctx context.Context, conversationID string) (*Ticket, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+ticketCols+` FROM tickets WHERE conversation_id = $1`, conversationID)
	if err != nil {
		return nil, err
	}
	t, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Ticket])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

// ApprovePlan approves one plan version: only while the ticket is planned
// and that version is the newest, so nobody approves a plan they did not
// read. With a clientID, a retry of an approval that succeeded returns
// success again (replayed) instead of ErrConflict.
func (s *Store) ApprovePlan(ctx context.Context, id string, version int, by, clientID string) (replayed bool, err error) {
	var agent string
	if err := s.Pool.QueryRow(ctx, `SELECT agent FROM tickets WHERE id = $1`, id).Scan(&agent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	req := map[string]any{"op": "approve_plan", "ticket": id, "version": version, "by": by}
	_, replayed, err = s.withReceipt(ctx, agent, clientID, req, func(tx pgx.Tx) (json.RawMessage, error) {
		_, from, err := lockTicket(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		var newest *int
		if err := tx.QueryRow(ctx, `SELECT max(version) FROM ticket_plans WHERE ticket_id = $1`, id).Scan(&newest); err != nil {
			return nil, err
		}
		if from != TicketPlanned || newest == nil || *newest != version {
			return nil, fmt.Errorf("ticket %s: plan %d is not the newest plan of a planned ticket: %w", id, version, ErrConflict)
		}
		if _, err := tx.Exec(ctx, `UPDATE tickets SET approved_plan = $2 WHERE id = $1`, id, version); err != nil {
			return nil, err
		}
		if err := setStatus(ctx, tx, id, from, TicketApproved, by, TicketUpdate{}, map[string]any{"plan": version}); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"ok":true}`), nil
	})
	return replayed, err
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
