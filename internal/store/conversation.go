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
// new. A fork's thread goes on into the conversation it was forked from, up
// to the fork point (ForkConversation), and so on for a fork of a fork.
func (s *Store) Conversation(ctx context.Context, agent, excludeRun string, limit int) ([]ConversationItem, error) {
	rows, err := s.Pool.Query(ctx, `
		WITH RECURSIVE line AS (
		    -- The run's conversation, then for a fork each conversation it
		    -- was forked from, up to where it was forked (upto).
		    SELECT c.id, c.forked_from, c.fork_cutoff, 'infinity'::timestamptz AS upto, 0 AS depth
		    FROM conversations c WHERE c.id = (SELECT conversation_id FROM runs WHERE id = $2)
		  UNION ALL
		    SELECT p.id, p.forked_from, p.fork_cutoff, least(l.upto, l.fork_cutoff), l.depth + 1
		    FROM line l JOIN conversations p ON p.id = l.forked_from
		    WHERE l.depth < $4
		)
		SELECT at, kind, run_id, summary, body FROM (
		    SELECT m.created_at AS at, 'message' AS kind, COALESCE(m.run_id::text, '') AS run_id,
		           CASE WHEN m.author LIKE 'agent:%' THEN m.author ELSE '' END AS summary, m.body
		    FROM messages m JOIN line l ON l.id = m.conversation_id AND m.created_at <= l.upto
		    WHERE m.agent = $1 AND m.direction = 'to_agent'
		      -- A parent's message before the fork point is the thread the
		      -- fork holds, read or not.
		      AND (l.depth > 0 OR (m.run_id <> $2 AND NOT (`+waitingMessage+`)))
		  UNION ALL
		    SELECT q.created_at, 'question', q.run_id::text, '', q.body
		    FROM messages q JOIN line l ON l.id = q.conversation_id AND q.created_at <= l.upto
		    WHERE q.agent = $1 AND q.direction = 'from_agent' AND q.run_id <> $2
		  UNION ALL
		    SELECT r.finished_at, CASE r.status WHEN 'succeeded' THEN 'report' ELSE 'failure' END, r.id::text,
		           COALESCE(r.summary, ''), COALESCE(CASE r.status WHEN 'succeeded' THEN r.report ELSE r.error END, '')
		    FROM runs r JOIN line l ON l.id = r.conversation_id AND r.finished_at <= l.upto
		    WHERE r.agent = $1 AND r.status IN ('succeeded', 'failed') AND r.finished_at IS NOT NULL AND r.id <> $2
		) t
		ORDER BY at DESC, kind LIMIT $3`, agent, excludeRun, limit, maxForkDepth)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ConversationItem])
}
