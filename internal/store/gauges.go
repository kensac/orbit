package store

import (
	"context"
	"time"
)

// OldestQueuedAge is how long the oldest due queued run has waited since it became due (its creation, or its run_after for a continuation); 0 when none is due. A queue no executor drains shows here while the queued count may stay small.
func (s *Store) OldestQueuedAge(ctx context.Context) (time.Duration, error) {
	var secs float64
	err := s.Pool.QueryRow(ctx, `
		SELECT coalesce(extract(epoch FROM now() - min(greatest(created_at, coalesce(run_after, created_at)))), 0)::float8
		FROM runs WHERE status = 'queued' AND (run_after IS NULL OR run_after <= now())`).Scan(&secs)
	if secs < 0 {
		secs = 0
	}
	return time.Duration(secs * float64(time.Second)), err
}
