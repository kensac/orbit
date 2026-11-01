package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrReceiptMismatch is a client request id reused for a different request.
var ErrReceiptMismatch = errors.New("client request id already used for a different request")

// withReceipt runs do in one transaction with the receipt for (agent,
// clientID). When clientID is empty it just runs do. When a receipt exists,
// do is not run: the stored response is returned with replayed = true. A
// concurrent duplicate waits on the first one's uncommitted receipt row and
// then reads its result, so two retries in flight still act once.
func (s *Store) withReceipt(ctx context.Context, agent, clientID string, request any,
	do func(tx pgx.Tx) (json.RawMessage, error)) (resp json.RawMessage, replayed bool, err error) {
	if clientID == "" {
		err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			resp, err = do(tx)
			return err
		})
		return resp, false, err
	}
	b, _ := json.Marshal(request)
	sum := sha256.Sum256(b)
	sha := hex.EncodeToString(sum[:])
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO receipts (agent, client_id, request_sha) VALUES ($1, $2, $3)
			ON CONFLICT (agent, client_id) DO NOTHING`, agent, clientID, sha)
		if isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var got string
			if err := tx.QueryRow(ctx, `SELECT request_sha, response FROM receipts WHERE agent = $1 AND client_id = $2`,
				agent, clientID).Scan(&got, &resp); err != nil {
				return err
			}
			if got != sha {
				return ErrReceiptMismatch
			}
			replayed = true
			return nil
		}
		if resp, err = do(tx); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE receipts SET response = $3 WHERE agent = $1 AND client_id = $2`, agent, clientID, []byte(resp))
		return err
	})
	return resp, replayed, err
}

// PostMessage is a person's message, and the run it wakes when wake is set.
type PostMessage struct {
	Agent   string
	SpecSHA string
	Body    string
	Author  string
	ReplyTo *string
	Wake    bool
	// ClientID makes the post idempotent (receipts).
	ClientID string
	// ConversationID is where the message goes; empty is the agent's
	// default conversation.
	ConversationID string
}

// PostResult is what PostMessage did.
type PostResult struct {
	MessageID string `json:"message_id"`
	RunID     string `json:"run_id,omitempty"`
}

// PostMessageOnce stores the message and enqueues its run in one
// transaction, so a retry with the same ClientID neither duplicates the
// message nor wakes a second run. replayed reports a retry.
func (s *Store) PostMessageOnce(ctx context.Context, p PostMessage) (res PostResult, replayed bool, err error) {
	req := map[string]any{"op": "message", "conversation": p.ConversationID, "body": p.Body, "reply_to": p.ReplyTo,
		"author": p.Author, "wake": p.Wake}
	raw, replayed, err := s.withReceipt(ctx, p.Agent, p.ClientID, req, func(tx pgx.Tx) (json.RawMessage, error) {
		r, err := postMessage(ctx, tx, p)
		if err != nil {
			return nil, err
		}
		return json.Marshal(r)
	})
	if err != nil {
		return res, false, err
	}
	return res, replayed, json.Unmarshal(raw, &res)
}

// ErrReplyElsewhere is a reply_to naming a message of another conversation.
var ErrReplyElsewhere = errors.New("reply_to names a message in another conversation")

// postMessage writes a person's message into its conversation and the run
// it wakes. It brings an archived conversation back, and names an untitled
// one after the message (DefaultTitle).
func postMessage(ctx context.Context, tx pgx.Tx, p PostMessage) (PostResult, error) {
	var r PostResult
	conv := p.ConversationID
	if conv == "" {
		err := tx.QueryRow(ctx, `SELECT id::text FROM conversations WHERE agent = $1 AND is_default`, p.Agent).Scan(&conv)
		if errors.Is(err, pgx.ErrNoRows) {
			return r, ErrNotFound
		}
		if err != nil {
			return r, err
		}
	}
	if p.ReplyTo != nil {
		var same bool
		err := tx.QueryRow(ctx, `SELECT conversation_id = $2::uuid FROM messages WHERE id = $1`, *p.ReplyTo, conv).Scan(&same)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !same) {
			return r, ErrReplyElsewhere
		}
		if err != nil {
			return r, err
		}
	}
	if err := ticketOnMessage(ctx, tx, conv, p.Author); err != nil {
		return r, err
	}
	err := tx.QueryRow(ctx, `
		INSERT INTO messages (agent, direction, body, author, reply_to, conversation_id)
		VALUES ($1, 'to_agent', $2, NULLIF($3, ''), $4, $5) RETURNING id::text`,
		p.Agent, p.Body, p.Author, p.ReplyTo, conv).Scan(&r.MessageID)
	if isForeignKeyViolation(err) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if _, err := tx.Exec(ctx, `UPDATE conversations SET archived = false,
		title = CASE WHEN title = '' THEN $2 ELSE title END WHERE id = $1`, conv, DefaultTitle(p.Body)); err != nil {
		return r, err
	}
	if p.Wake {
		if err := tx.QueryRow(ctx, `INSERT INTO runs (agent, spec_sha, trigger, conversation_id) VALUES ($1, $2, 'message', $3) RETURNING id::text`,
			p.Agent, p.SpecSHA, conv).Scan(&r.RunID); err != nil {
			return r, err
		}
	}
	return r, nil
}

// DecideActionOnce is DecideAction with an optional client request id: a
// retry of a decision that succeeded returns success again instead of
// ErrConflict.
func (s *Store) DecideActionOnce(ctx context.Context, id string, approve bool, by, clientID string) (replayed bool, err error) {
	var agent string
	if err := s.Pool.QueryRow(ctx, `SELECT agent FROM actions WHERE id = $1`, id).Scan(&agent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	req := map[string]any{"op": "decide", "action": id, "approve": approve, "by": by}
	_, replayed, err = s.withReceipt(ctx, agent, clientID, req, func(tx pgx.Tx) (json.RawMessage, error) {
		if err := decideAction(ctx, tx, id, approve, by); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"ok":true}`), nil
	})
	return replayed, err
}

// decideAction is the guarded pending -> approved | rejected transition.
func decideAction(ctx context.Context, db execer, id string, approve bool, by string) error {
	status := "rejected"
	if approve {
		status = "approved"
	}
	tag, err := db.Exec(ctx, `UPDATE actions SET status = $2, decided_by = $3, decided_at = now()
		WHERE id = $1 AND status = 'pending'`, id, status, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %s: %w (not pending)", id, ErrConflict)
	}
	return nil
}
