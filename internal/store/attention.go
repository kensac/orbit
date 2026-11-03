package store

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Attention kinds: what a person is being asked for.
const (
	AttentionApproval   = "approval"   // an action waits for approve or reject
	AttentionQuestion   = "question"   // a run asked something
	AttentionFailure    = "failure"    // a run failed
	AttentionDecision   = "decision"   // a ticket's plan, or a delegated task's decision, waits for a person
	AttentionPermission = "permission" // a chat run's Claude Code asks to use a tool or to leave plan mode (approvals)
	AttentionBudget     = "budget"     // a scheduled run reached an agent's period budget
)

// Attention is one thing waiting on a person (migration 20261102032140). Title is
// all a notification may carry; Detail (the question, the action's reason,
// the error, the ticket's title) is for the signed-in inbox only.
type Attention struct {
	ID             string    `json:"id"`
	Key            string    `json:"key"`
	Kind           string    `json:"kind"`
	Agent          string    `json:"agent"`
	RunID          string    `json:"run_id"`
	ConversationID string    `json:"conversation_id"`
	ActionID       *string   `json:"action_id"`
	MessageID      *string   `json:"message_id"`
	TicketID       *string   `json:"ticket_id"`
	PlanVersion    *int      `json:"plan_version"`
	ApprovalID     *string   `json:"approval_id"`
	Title          string    `json:"title"`
	Detail         string    `json:"detail"`
	Push           bool      `json:"push"`
	CreatedAt      time.Time `json:"created_at"`
}

// newAttention records that a run's outcome needs a person. Its key is the
// identity of the thing (one row per action, question, failed run, plan
// version), so writing it twice is harmless. The title is the agent's name
// and suffix, nothing the run wrote. A run answering a person's message is
// recorded without push, as engine.Attended keeps its notifications in the
// thread.
func newAttention(ctx context.Context, db execer, kind, key, runID, suffix string, actionID, messageID, ticketID *string, plan *int) error {
	_, err := db.Exec(ctx, `
		INSERT INTO attention (key, kind, agent, run_id, conversation_id, action_id, message_id, ticket_id, plan_version, title, push)
		SELECT $1, $2, r.agent, r.id, r.conversation_id, $4::uuid, $5::uuid, $6::uuid, $7, r.agent || $8, r.trigger <> 'message'
		FROM runs r WHERE r.id = $3::uuid
		ON CONFLICT (key) DO NOTHING`, key, kind, runID, actionID, messageID, ticketID, plan, suffix)
	return err
}

// attentionOpen is true of an attention row (alias a) whose thing still
// waits: the action is pending, the question unanswered (openQuestion), the
// plan the ticket's newest while it is planned (a delegated task's
// decision: its task:decision action pending), and a failure stands until a
// later run of its conversation succeeds or the person writes there. Any
// row a person dismissed is closed. A permission request is open while its
// approval is pending and unexpired: a decision, a withdrawal and a timeout
// all close it.
const attentionOpen = `a.dismissed_at IS NULL AND CASE a.kind
	WHEN 'approval' THEN EXISTS (SELECT 1 FROM actions x WHERE x.id = a.action_id AND x.status = 'pending')
	WHEN 'question' THEN EXISTS (SELECT 1 FROM messages m WHERE m.id = a.message_id AND ` + openQuestion + `)
	WHEN 'decision' THEN CASE WHEN a.ticket_id IS NOT NULL
	    THEN EXISTS (SELECT 1 FROM tickets t WHERE t.id = a.ticket_id AND t.status = 'planned'
	                 AND a.plan_version = (SELECT max(p.version) FROM ticket_plans p WHERE p.ticket_id = t.id))
	    ELSE EXISTS (SELECT 1 FROM actions x WHERE x.id = a.action_id AND x.status = 'pending') END
	WHEN 'failure' THEN NOT EXISTS (SELECT 1 FROM runs l WHERE l.conversation_id = a.conversation_id AND l.agent = a.agent
	                                AND l.status = 'succeeded' AND l.created_at > (SELECT f.created_at FROM runs f WHERE f.id = a.run_id))
	                AND NOT EXISTS (SELECT 1 FROM messages p WHERE p.conversation_id = a.conversation_id AND p.direction = 'to_agent'
	                                AND p.created_at > (SELECT COALESCE(f.finished_at, f.created_at) FROM runs f WHERE f.id = a.run_id))
	WHEN 'permission' THEN EXISTS (SELECT 1 FROM approvals p WHERE p.id = a.approval_id AND p.state = 'pending' AND p.expires_at > now())
	WHEN 'budget' THEN true
	ELSE false END`

const attentionCols = `a.id::text, a.key, a.kind, a.agent, a.run_id::text, a.conversation_id::text, a.action_id::text,
	a.message_id::text, a.ticket_id::text, a.plan_version, a.approval_id::text, a.title`

// ListAttention returns what is open, newest first, with each thing's
// detail.
func (s *Store) ListAttention(ctx context.Context, limit int) ([]Attention, error) {
	limit = clampLimit(limit, 500, 500)
	rows, err := s.Pool.Query(ctx, `
		SELECT `+attentionCols+`,
		       COALESCE(CASE a.kind
		           WHEN 'approval' THEN (SELECT x.reason FROM actions x WHERE x.id = a.action_id)
		           WHEN 'question' THEN (SELECT m.body FROM messages m WHERE m.id = a.message_id)
		           WHEN 'failure' THEN (SELECT r.error FROM runs r WHERE r.id = a.run_id)
	           WHEN 'budget' THEN (SELECT r.summary FROM runs r WHERE r.id = a.run_id)
		           WHEN 'decision' THEN COALESCE((SELECT t.title FROM tickets t WHERE t.id = a.ticket_id),
		                                         (SELECT x.reason FROM actions x WHERE x.id = a.action_id))
		           WHEN 'permission' THEN (SELECT p.tool FROM approvals p WHERE p.id = a.approval_id)
		       END, ''),
		       a.push, a.created_at
		FROM attention a
		WHERE `+attentionOpen+`
		ORDER BY a.created_at DESC, a.id
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Attention])
}

// DismissAttention closes one row for whoever dismissed it, whatever its
// thing's state (a pending action stays pending; it just leaves the inbox).
func (s *Store) DismissAttention(ctx context.Context, id, by string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE attention SET dismissed_at = now(), dismissed_by = $2
		WHERE id = $1 AND dismissed_at IS NULL`, id, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		var exists bool
		if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attention WHERE id = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

// pushLock serialises ClaimPush across control instances (a rollout runs
// two), so a burst is folded once and the gap holds fleet-wide.
const pushLock = 0x6f72626974707368 // "orbitpsh"

// ClaimPush takes every row waiting to be pushed and marks it handled, in
// one transaction, unless a push went out less than gap ago (the rows wait
// and are folded into the next one). Rows that closed before their turn,
// or are older than maxAge (control was down), are marked and not
// returned. The caller sends what is returned; a send that fails is not
// retried, as the inbox still lists the thing.
func (s *Store) ClaimPush(ctx context.Context, gap, maxAge time.Duration) ([]Attention, error) {
	var out []Attention
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(pushLock)); err != nil {
			return err
		}
		var recent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM push_state WHERE last_push_at > now() - $1::interval)`,
			gap.String()).Scan(&recent); err != nil {
			return err
		}
		if recent {
			return nil
		}
		rows, err := tx.Query(ctx, `
			UPDATE attention a SET pushed_at = now()
			WHERE a.push AND a.pushed_at IS NULL
			RETURNING `+attentionCols+`, '', a.push, a.created_at,
			          (`+attentionOpen+`) AND a.created_at > now() - $1::interval`, maxAge.String())
		if err != nil {
			return err
		}
		type claimed struct {
			Attention
			Open bool
		}
		list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (claimed, error) {
			var c claimed
			a := &c.Attention
			err := row.Scan(&a.ID, &a.Key, &a.Kind, &a.Agent, &a.RunID, &a.ConversationID, &a.ActionID, &a.MessageID,
				&a.TicketID, &a.PlanVersion, &a.ApprovalID, &a.Title, &a.Detail, &a.Push, &a.CreatedAt, &c.Open)
			return c, err
		})
		if err != nil {
			return err
		}
		for _, c := range list {
			if c.Open {
				out = append(out, c.Attention)
			}
		}
		if len(out) == 0 {
			return nil
		}
		_, err = tx.Exec(ctx, `INSERT INTO push_state (id, last_push_at) VALUES (true, now())
			ON CONFLICT (id) DO UPDATE SET last_push_at = now()`)
		return err
	})
	if err != nil {
		return nil, err
	}
	// Oldest first, so a single item and a summary read in order.
	slices.SortFunc(out, func(x, y Attention) int { return x.CreatedAt.Compare(y.CreatedAt) })
	return out, nil
}

// ---------------------------------------------------------------- devices

// PushSubscription is a device's Web Push subscription. P256DH and Auth are
// the browser's keys for encrypting to it; they are never returned by the
// API.
type PushSubscription struct {
	ID         string     `json:"id"`
	Endpoint   string     `json:"endpoint"`
	P256DH     string     `json:"-"`
	Auth       string     `json:"-"`
	Author     string     `json:"author"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSentAt *time.Time `json:"last_sent_at"`
	LastError  *string    `json:"last_error"`
	Failures   int        `json:"failures"`
}

const pushCols = `id::text, endpoint, p256dh, auth, author, label, created_at, last_sent_at, last_error, failures`

// SavePushSubscription records a device, or refreshes it (new keys, a
// revoked one revived) when its endpoint is already known.
func (s *Store) SavePushSubscription(ctx context.Context, p PushSubscription) (string, error) {
	if p.Endpoint == "" || p.P256DH == "" || p.Auth == "" || p.Author == "" {
		return "", fmt.Errorf("%w: endpoint, keys and author are required", ErrInvalid)
	}
	var id string
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO push_subscriptions (endpoint, p256dh, auth, author, label) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (endpoint) DO UPDATE SET p256dh = EXCLUDED.p256dh, auth = EXCLUDED.auth, author = EXCLUDED.author,
		    label = EXCLUDED.label, updated_at = now(), revoked_at = NULL, failures = 0, last_error = NULL
		RETURNING id::text`, p.Endpoint, p.P256DH, p.Auth, p.Author, p.Label).Scan(&id)
	return id, err
}

// RevokePushSubscription stops pushing to a device. ErrNotFound means no
// active subscription has that endpoint.
func (s *Store) RevokePushSubscription(ctx context.Context, endpoint string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE push_subscriptions SET revoked_at = now(), updated_at = now()
		WHERE endpoint = $1 AND revoked_at IS NULL`, endpoint)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ListPushSubscriptions returns the active devices, of one author or of
// everyone when author is empty.
func (s *Store) ListPushSubscriptions(ctx context.Context, author string) ([]PushSubscription, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+pushCols+` FROM push_subscriptions
		WHERE revoked_at IS NULL AND ($1 = '' OR author = $1) ORDER BY created_at`, author)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[PushSubscription])
}

// PushResult records one send to a device. gone (the push service answered
// 404 or 410) revokes it: the browser dropped the subscription.
func (s *Store) PushResult(ctx context.Context, id string, sendErr error, gone bool) error {
	if sendErr == nil {
		_, err := s.Pool.Exec(ctx, `UPDATE push_subscriptions SET last_sent_at = now(), failures = 0, last_error = NULL
			WHERE id = $1`, id)
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE push_subscriptions SET failures = failures + 1, last_error = $2,
		revoked_at = CASE WHEN $3 THEN now() ELSE revoked_at END WHERE id = $1`, id, sendErr.Error(), gone)
	return err
}
