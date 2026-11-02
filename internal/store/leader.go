package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// schedulerLock is the scheduler leader's advisory lock id: "orbit" in ASCII.
const schedulerLock = 0x6f72626974

// SchedulerLock is the scheduler leader's advisory lock, held on one pooled
// connection for the leader's lifetime: if that connection drops, Postgres
// releases the lock, which Ping reports.
type SchedulerLock struct {
	conn *pgxpool.Conn
}

// TrySchedulerLock takes the scheduler's lock, or returns nil without
// waiting when another instance holds it.
func (s *Store) TrySchedulerLock(ctx context.Context) (*SchedulerLock, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(schedulerLock)).Scan(&ok); err != nil {
		conn.Release()
		return nil, err
	}
	if !ok {
		conn.Release()
		return nil, nil
	}
	return &SchedulerLock{conn: conn}, nil
}

// Ping keeps the lock's connection alive; an error means the lock may be
// lost.
func (l *SchedulerLock) Ping(ctx context.Context) error {
	return l.conn.Ping(ctx)
}

// Release unlocks and returns the connection to the pool. The unlock is
// explicit because a pooled connection outlives Release, and would
// otherwise keep the lock until the process exits.
func (l *SchedulerLock) Release(ctx context.Context) {
	_, _ = l.conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(schedulerLock))
	l.conn.Release()
}
