package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Steering (docs/ARCHITECTURE.md, Steering a running reply): a person's message with delivery steer goes into the turn the conversation's running run is in. The attempt running it reads the unread steer messages (PendingSteers) while its turn runs, writes them into it, and marks each the CLI took (MarkSteered). One it did not take stays waiting (waitingMessage), and the run's end wakes a run for it (wakeForSteers, in finishRun), so no steer is left unanswered.

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

// PendingSteers are the steer messages of the run's conversation waiting for a run (waitingMessage), oldest first.
func (s *Store) PendingSteers(ctx context.Context, runID string) ([]Message, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT m.id::text, m.agent, m.direction, m.body, m.reply_to::text, m.run_id::text, m.author, m.created_at
		FROM runs r JOIN messages m ON m.conversation_id = r.conversation_id
		WHERE r.id = $1 AND m.delivery = 'steer' AND `+waitingMessage+`
		ORDER BY m.created_at, m.id`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Message])
}

// MarkSteered records that the attempt's turn took a steer message: read by the attempt (message_reads, as StartRunContext reads) and steered_into the run. ErrConflict when the message is no longer waiting, or the attempt no longer holds the run.
func (s *Store) MarkSteered(ctx context.Context, runID string, attempt int, messageID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE messages m SET run_id = r.id, steered_into = r.id
			FROM runs r
			WHERE m.id = $3 AND m.delivery = 'steer' AND `+waitingMessage+`
			  AND r.id = $1 AND r.status = 'running' AND r.attempt = $2 AND r.conversation_id = m.conversation_id`,
			runID, attempt, messageID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("steer message %s into run %s attempt %d: %w", messageID, runID, attempt, ErrConflict)
		}
		if _, err := tx.Exec(ctx, `UPDATE message_reads SET released_at = now() WHERE message_id = $1 AND released_at IS NULL`, messageID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO message_reads (message_id, run_id, attempt) VALUES ($1, $2, $3)`, messageID, runID, attempt)
		return err
	})
}

// wakeForSteers queues a run for the steer messages a finishing run's turn did not take, unless the conversation already has one queued, which reads them. It is the run queueing them would have woken: a person's message wakes a 'message' run (attended), and a parent agent's alone (task:message, MessageTask) a 'task' run, so a parent's steer never runs as a person's instruction.
func wakeForSteers(ctx context.Context, tx pgx.Tx, runID string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO runs (agent, spec_sha, trigger, conversation_id)
		SELECT r.agent, r.spec_sha,
		       CASE WHEN EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = r.conversation_id
		                         AND m.delivery = 'steer' AND (m.author IS NULL OR m.author NOT LIKE '`+AgentAuthorPrefix+`%') AND `+waitingMessage+`)
		            THEN 'message' ELSE 'task' END,
		       r.conversation_id FROM runs r
		WHERE r.id = $1
		  AND EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = r.conversation_id
		              AND m.delivery = 'steer' AND `+waitingMessage+`)
		  AND NOT EXISTS (SELECT 1 FROM runs q WHERE q.conversation_id = r.conversation_id AND q.status = 'queued')`, runID)
	return err
}
