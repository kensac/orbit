package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConversationItem is one turn of an agent's thread as a run's prompt shows
// it: something the person said, a report the agent gave, or a question it
// asked.
type ConversationItem struct {
	At time.Time `json:"at"`
	// Kind is message (from the person), report (a run's summary and
	// report), failure (a run's error) or question (from the agent).
	Kind  string `json:"kind"`
	RunID string `json:"run_id,omitempty"`
	// Summary is a report's one line, or for a message an agent wrote
	// (delegation) its author, agent:<name>; Body is the message, the
	// report, the error or the question.
	Summary string `json:"summary,omitempty"`
	Body    string `json:"body"`
}

// Conversation returns up to limit of the agent's most recent thread items
// from before the run excludeRun, newest first: the person's messages that
// earlier runs read, the reports and errors of finished runs, and the
// questions those runs asked, all from excludeRun's own conversation.
// Messages excludeRun itself consumed are left out; the prompt shows them as
// new.
func (s *Store) Conversation(ctx context.Context, agent, excludeRun string, limit int) ([]ConversationItem, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH me AS (SELECT conversation_id FROM runs WHERE id = $2)
		SELECT at, kind, run_id, summary, body FROM (
		    SELECT m.created_at AS at, 'message' AS kind, COALESCE(m.run_id::text, '') AS run_id, 
		           CASE WHEN m.author LIKE 'agent:%' THEN m.author ELSE '' END AS summary, m.body
		    FROM messages m
		    WHERE m.agent = $1 AND m.direction = 'to_agent' AND m.run_id <> $2 AND NOT (`+waitingMessage+`)
		      AND m.conversation_id = (SELECT conversation_id FROM me)
		  UNION ALL
		    SELECT q.created_at, 'question', q.run_id::text, '', q.body
		    FROM messages q
		    WHERE q.agent = $1 AND q.direction = 'from_agent' AND q.run_id <> $2
		      AND q.conversation_id = (SELECT conversation_id FROM me)
		  UNION ALL
		    SELECT r.finished_at, CASE r.status WHEN 'succeeded' THEN 'report' ELSE 'failure' END, r.id::text,
		           COALESCE(r.summary, ''), COALESCE(CASE r.status WHEN 'succeeded' THEN r.report ELSE r.error END, '')
		    FROM runs r
		    WHERE r.agent = $1 AND r.status IN ('succeeded', 'failed') AND r.finished_at IS NOT NULL AND r.id <> $2
		      AND r.conversation_id = (SELECT conversation_id FROM me)
		) t
		ORDER BY at DESC, kind LIMIT $3`, agent, excludeRun, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ConversationItem])
}
