package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TriggerLearn is a learning run's trigger: it reads one conversation since the last learning run of it and writes memory entries.
const TriggerLearn = "learn"

// LearningDue is a conversation ready to be learned from.
type LearningDue struct {
	ConversationID string
	// Through is the newest of the person's messages in it: the point the learning run covers up to.
	Through time.Time
}

// LearningDue lists an agent's conversations that are quiet and hold something new from the person: a message after the last succeeded learning run's point, no message or run start in idle, no run finished in idle, none queued or running, and no learning run already queued, running or tried for that point. A conversation of its own is never learned from (the learning conversation, purpose set).
func (s *Store) LearningDue(ctx context.Context, agent string, idle time.Duration) ([]LearningDue, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT c.id::text, last.at
		FROM conversations c
		CROSS JOIN LATERAL (
		    SELECT max(m.created_at) AS at FROM messages m
		    WHERE m.conversation_id = c.id AND m.direction = 'to_agent') last
		WHERE c.agent = $1 AND c.purpose IS NULL AND last.at IS NOT NULL
		  AND c.last_activity_at < now() - $2::interval
		  AND NOT EXISTS (
		      SELECT 1 FROM runs r WHERE r.conversation_id = c.id
		        AND (r.status IN ('queued', 'running') OR r.finished_at > now() - $2::interval))
		  AND NOT EXISTS (
		      SELECT 1 FROM runs l WHERE l.trigger = 'learn' AND l.learns_from = c.id
		        AND (l.status IN ('queued', 'running') OR l.learn_through >= last.at))
		ORDER BY last.at`, agent, idle)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[LearningDue])
}

// EnqueueLearning queues a learning run of conversation through its point, in the agent's learning conversation (created on first use, archived). It returns "" when one for that point exists already (runs_one_learning), so callers on several instances enqueue it once.
func (s *Store) EnqueueLearning(ctx context.Context, agent, specSHA, conversationID string, through time.Time) (string, error) {
	var id string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO conversations (agent, title, archived, purpose)
			VALUES ($1, 'Learning', true, 'learning') ON CONFLICT (agent) WHERE purpose = 'learning' DO NOTHING`, agent); err != nil {
			if isForeignKeyViolation(err) {
				return ErrNotFound
			}
			return err
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO runs (agent, spec_sha, trigger, conversation_id, learns_from, learn_through)
			SELECT $1, $2, 'learn', l.id, $3, $4 FROM conversations l
			WHERE l.agent = $1 AND l.purpose = 'learning'
			  AND EXISTS (SELECT 1 FROM conversations c WHERE c.id = $3 AND c.agent = $1 AND c.purpose IS NULL)
			ON CONFLICT (learns_from, learn_through) WHERE trigger = 'learn' DO NOTHING
			RETURNING id::text`, agent, specSHA, conversationID, through).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			id = ""
			return nil
		}
		return err
	})
	return id, err
}

// LearningWindow is what a learning run reads: its conversation, and the point the previous succeeded learning run of it covered (nil for none).
type LearningWindow struct {
	ConversationID string
	After          *time.Time
	Through        time.Time
}

// LearningWindowOf returns a learning run's window.
func (s *Store) LearningWindowOf(ctx context.Context, runID string) (*LearningWindow, error) {
	var w LearningWindow
	err := s.Pool.QueryRow(ctx, `
		SELECT r.learns_from::text, r.learn_through,
		       (SELECT max(p.learn_through) FROM runs p
		        WHERE p.trigger = 'learn' AND p.learns_from = r.learns_from AND p.status = 'succeeded' AND p.learn_through < r.learn_through)
		FROM runs r WHERE r.id = $1 AND r.trigger = 'learn'`, runID).Scan(&w.ConversationID, &w.Through, &w.After)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &w, err
}

// LearningItems returns up to limit of a conversation's items after a point, newest first: the person's messages up to through, the agent's questions, and its runs' reports and errors (a reply to the last message comes after through, and is read too).
func (s *Store) LearningItems(ctx context.Context, w LearningWindow, limit int) ([]ConversationItem, error) {
	var after any
	if w.After != nil {
		after = *w.After
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT at, kind, run_id, summary, body FROM (
		    SELECT m.created_at AS at, 'message' AS kind, COALESCE(m.run_id::text, '') AS run_id, '' AS summary, m.body
		    FROM messages m
		    WHERE m.conversation_id = $1 AND m.direction = 'to_agent' AND m.created_at <= $3
		      AND m.created_at > COALESCE($2::timestamptz, '-infinity')
		  UNION ALL
		    SELECT q.created_at, 'question', q.run_id::text, '', q.body
		    FROM messages q
		    WHERE q.conversation_id = $1 AND q.direction = 'from_agent' AND q.created_at > COALESCE($2::timestamptz, '-infinity')
		  UNION ALL
		    SELECT r.finished_at, CASE r.status WHEN 'succeeded' THEN 'report' ELSE 'failure' END, r.id::text,
		           COALESCE(r.summary, ''), COALESCE(CASE r.status WHEN 'succeeded' THEN r.report ELSE r.error END, '')
		    FROM runs r
		    WHERE r.conversation_id = $1 AND r.status IN ('succeeded', 'failed') AND r.finished_at IS NOT NULL
		      AND r.finished_at > COALESCE($2::timestamptz, '-infinity')
		) t
		ORDER BY at DESC, kind LIMIT $4`, w.ConversationID, after, w.Through, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ConversationItem])
}

// LearnNow queues a learning run of a conversation through its newest message from the person, whatever the quiet: the API's way to learn from a conversation at once. ErrConflict when there is nothing new since the last learning run, or one for that point exists.
func (s *Store) LearnNow(ctx context.Context, conversationID, specSHA string) (string, error) {
	if !IsUUID(conversationID) {
		return "", ErrNotFound
	}
	var agent string
	var through *time.Time
	var purpose *string
	err := s.Pool.QueryRow(ctx, `SELECT c.agent, c.purpose,
		    (SELECT max(m.created_at) FROM messages m WHERE m.conversation_id = c.id AND m.direction = 'to_agent')
		FROM conversations c WHERE c.id = $1`, conversationID).Scan(&agent, &purpose, &through)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if purpose != nil {
		return "", fmt.Errorf("%w: the learning conversation is not learned from", ErrInvalid)
	}
	if through == nil {
		return "", fmt.Errorf("nothing from the person to learn from: %w", ErrConflict)
	}
	id, err := s.EnqueueLearning(ctx, agent, specSHA, conversationID, *through)
	if err == nil && id == "" {
		err = fmt.Errorf("a learning run for this point exists already: %w", ErrConflict)
	}
	return id, err
}
