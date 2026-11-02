package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Steering (docs/ARCHITECTURE.md, Steering a running reply): a person's message with delivery steer goes into the turn the conversation's running run is in. The attempt running it reads the unread steer messages (PendingSteers) while its turn runs, writes them into it, and marks each the CLI took (MarkSteered). One it did not take stays unread, and the run's end wakes a run for it (wakeForSteers, in finishRun), so no steer is left unanswered.

// steerTarget is the conversation's running run a steer goes to: one a person has not asked to stop, and not a build or a learning run, which no message reaches. It is locked FOR SHARE, so a run finishing at the same moment either finishes after this transaction commits (and its finishRun sees the message, waking a run for it if its turn did not take it) or before it (and the message is queued). "" when none.
func steerTarget(ctx context.Context, tx pgx.Tx, conversationID string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM runs
		WHERE conversation_id = $1 AND status = 'running' AND cancel_requested_at IS NULL AND trigger NOT IN ('build', 'learn')
		ORDER BY started_at DESC LIMIT 1 FOR SHARE`, conversationID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// PendingSteers are the steer messages of the run's conversation no run has read yet, oldest first.
func (s *Store) PendingSteers(ctx context.Context, runID string) ([]Message, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT m.id::text, m.agent, m.direction, m.body, m.reply_to::text, m.run_id::text, m.author, m.created_at
		FROM runs r JOIN messages m ON m.conversation_id = r.conversation_id
		WHERE r.id = $1 AND m.direction = 'to_agent' AND m.run_id IS NULL AND m.delivery = 'steer'
		ORDER BY m.created_at, m.id`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Message])
}

// MarkSteered records that the attempt's turn took a steer message: it is read by the run and steered_into it. ErrConflict when the message is no longer unread, or the attempt no longer holds the run.
func (s *Store) MarkSteered(ctx context.Context, runID string, attempt int, messageID string) error {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE messages m SET run_id = r.id, steered_into = r.id
		FROM runs r
		WHERE m.id = $3 AND m.direction = 'to_agent' AND m.run_id IS NULL AND m.delivery = 'steer'
		  AND r.id = $1 AND r.status = 'running' AND r.attempt = $2 AND r.conversation_id = m.conversation_id`,
		runID, attempt, messageID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("steer message %s into run %s attempt %d: %w", messageID, runID, attempt, ErrConflict)
	}
	return nil
}

// wakeForSteers queues a run for the steer messages a finishing run's turn did not take, unless the conversation already has one queued, which reads them.
func wakeForSteers(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO runs (agent, spec_sha, trigger, conversation_id)
		SELECT r.agent, r.spec_sha, 'message', r.conversation_id FROM runs r
		WHERE r.id = $1
		  AND EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = r.conversation_id
		              AND m.direction = 'to_agent' AND m.run_id IS NULL AND m.delivery = 'steer')
		  AND NOT EXISTS (SELECT 1 FROM runs q WHERE q.conversation_id = r.conversation_id AND q.status = 'queued')`, runID)
	return err
}
