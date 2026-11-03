package store

import (
	"context"
	"time"
)

// SetPresence records the conversation a person's app is showing now (nil: none), replacing what it showed before (migration presence). The time is the database's, so instances with skewed clocks agree on freshness.
func (s *Store) SetPresence(ctx context.Context, person string, conversationID *string) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO presence (person, conversation_id, seen_at) VALUES ($1, $2::uuid, now())
		ON CONFLICT (person) DO UPDATE SET conversation_id = EXCLUDED.conversation_id, seen_at = now()`, person, conversationID)
	return err
}

// Watching returns, for each person whose app reported a conversation less than fresh ago, that conversation. A person absent from it is not known to be looking at anything.
func (s *Store) Watching(ctx context.Context, fresh time.Duration) (map[string]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT person, conversation_id::text FROM presence
		WHERE conversation_id IS NOT NULL AND seen_at > now() - $1::interval`, fresh.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, c string
		if err := rows.Scan(&p, &c); err != nil {
			return nil, err
		}
		out[p] = c
	}
	return out, rows.Err()
}
