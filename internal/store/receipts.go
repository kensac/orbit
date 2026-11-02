package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/kensac/orbit/internal/forms"
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
	// Attachments are uploads of the conversation the message carries.
	Attachments []string
	// Answers fill the form of the question ReplyTo names (forms.Answers); nil for a message without.
	Answers map[string]json.RawMessage
	// Delivery is DeliveryQueue (also when empty) or DeliverySteer.
	Delivery string
	// Steerable is true when the agent's runs can take a message mid-turn (a chat agent); a steer to any other is queued, as is one with attachments (their files reach a run's workspace when it starts).
	Steerable bool
}

// Deliveries of a person's message (messages.delivery). A queued message waits for the conversation's next run (and wakes one when asked). A steered one goes into the turn the conversation's running run is in (engine, steer.go); with no run running, or for an agent whose runs cannot take one, it is queued instead.
const (
	DeliveryQueue = "queue"
	DeliverySteer = "steer"
)

// PostResult is what PostMessage did.
type PostResult struct {
	MessageID string `json:"message_id"`
	// RunID is the run the message woke, or for a steered message the running run it was handed to.
	RunID string `json:"run_id,omitempty"`
	// Delivered is how the message was taken: "queued" or "steered".
	Delivered string `json:"delivered"`
}

// Delivered values: what a post did with the message.
const (
	DeliveredQueued  = "queued"
	DeliveredSteered = "steered"
)

// PostMessageOnce stores the message and enqueues its run in one
// transaction, so a retry with the same ClientID neither duplicates the
// message nor wakes a second run. replayed reports a retry.
func (s *Store) PostMessageOnce(ctx context.Context, p PostMessage) (res PostResult, replayed bool, err error) {
	req := map[string]any{"op": "message", "conversation": p.ConversationID, "body": p.Body, "reply_to": p.ReplyTo,
		"author": p.Author, "wake": p.Wake, "attachments": p.Attachments, "answers": p.Answers}
	if p.Delivery == DeliverySteer {
		// Only a steer is named, so a receipt stored before delivery existed still matches its retry.
		req["delivery"] = p.Delivery
	}
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
	if err := json.Unmarshal(raw, &res); err != nil {
		return res, replayed, err
	}
	if res.Delivered == "" {
		// A receipt stored before deliveries existed: every message then was queued.
		res.Delivered = DeliveredQueued
	}
	return res, replayed, nil
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
	var answers []byte
	if p.Answers != nil {
		var err error
		if p.Body, p.Attachments, answers, err = answerForm(ctx, tx, conv, p); err != nil {
			return r, err
		}
	}
	if err := ticketOnMessage(ctx, tx, conv, p.Author); err != nil {
		return r, err
	}
	delivery := DeliveryQueue
	r.Delivered = DeliveredQueued
	if p.Delivery == DeliverySteer && p.Steerable && len(p.Attachments) == 0 {
		running, err := steerTarget(ctx, tx, conv)
		if err != nil {
			return r, err
		}
		if running != "" {
			r.Delivered, r.RunID, delivery = DeliveredSteered, running, DeliverySteer
		}
	}
	// A queued message without answers is written as every message was before deliveries and forms existed: delivery is the column's default, and answers is named only when there are some (TestSearchBackfillsRowsWrittenBeforeIt writes through an older schema).
	cols, vals := "agent, direction, body, author, reply_to, conversation_id", "$1, 'to_agent', $2, NULLIF($3, ''), $4, $5"
	args := []any{p.Agent, p.Body, p.Author, p.ReplyTo, conv}
	if delivery == DeliverySteer {
		cols, vals = cols+", delivery", vals+", 'steer'"
	}
	if answers != nil {
		args = append(args, answers)
		cols, vals = cols+", answers", vals+fmt.Sprintf(", $%d", len(args))
	}
	insert := `INSERT INTO messages (` + cols + `) VALUES (` + vals + `) RETURNING id::text`
	err := tx.QueryRow(ctx, insert, args...).Scan(&r.MessageID)
	if isForeignKeyViolation(err) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	if err := attach(ctx, tx, r.MessageID, conv, p.Attachments); err != nil {
		return r, err
	}
	if _, err := tx.Exec(ctx, `UPDATE conversations SET archived = false,
		title = CASE WHEN title = '' THEN $2 ELSE title END WHERE id = $1`, conv, DefaultTitle(p.Body)); err != nil {
		return r, err
	}
	if p.Wake && delivery == DeliveryQueue {
		if err := tx.QueryRow(ctx, `INSERT INTO runs (agent, spec_sha, trigger, conversation_id) VALUES ($1, $2, 'message', $3) RETURNING id::text`,
			p.Agent, p.SpecSHA, conv).Scan(&r.RunID); err != nil {
			return r, err
		}
	}
	return r, nil
}

// answerForm checks a reply's answers against the form of the question it names and returns the message as stored: the body the person typed followed by the answers as a numbered list (forms.Text, which is what the agent reads), the attachments with every file answer's upload added, and the answers as JSON. A file answer must be an upload of the conversation.
func answerForm(ctx context.Context, tx pgx.Tx, conv string, p PostMessage) (string, []string, []byte, error) {
	if p.ReplyTo == nil {
		return "", nil, nil, fmt.Errorf("%w: answers need reply_to, the question they answer", ErrInvalid)
	}
	var question string
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT body, fields FROM messages WHERE id = $1`, *p.ReplyTo).Scan(&question, &raw); err != nil {
		return "", nil, nil, err
	}
	if raw == nil {
		return "", nil, nil, fmt.Errorf("%w: answers: the question reply_to names has no form", ErrInvalid)
	}
	var fields []forms.Field
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", nil, nil, err
	}
	vals, err := forms.Answers(fields, p.Answers)
	if err != nil {
		return "", nil, nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	attachments := p.Attachments
	names := map[string]string{}
	for _, f := range fields {
		id, ok := vals[f.Name].(string)
		if !ok || f.Type != forms.File {
			continue
		}
		var name, of string
		err := tx.QueryRow(ctx, `SELECT name, conversation_id::text FROM uploads WHERE id = $1`, id).Scan(&name, &of)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && of != conv) {
			return "", nil, nil, fmt.Errorf("%w: answers: %s: %s is not an upload of this conversation", ErrInvalid, f.Name, id)
		}
		if err != nil {
			return "", nil, nil, err
		}
		names[id] = "uploads/" + name
		if !slices.Contains(attachments, id) {
			attachments = append(attachments, id)
		}
	}
	head := strings.TrimSpace(question)
	if i := strings.IndexByte(head, '\n'); i >= 0 {
		head = head[:i]
	}
	text := fmt.Sprintf("Answers to %q:\n%s", head, forms.Text(fields, vals, names))
	body := strings.TrimSpace(p.Body)
	if body != "" {
		body += "\n\n"
	}
	stored, err := json.Marshal(vals)
	return strings.TrimRight(body+text, "\n"), attachments, stored, err
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
