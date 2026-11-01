package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------- attempts

// Attempt is one claim of a run, as GET /runs/{id} reports it.
type Attempt struct {
	Attempt    int        `json:"attempt"`
	Worker     string     `json:"worker"`
	RunnerKind *string    `json:"runner_kind,omitempty"`
	SpecSHA    *string    `json:"spec_sha,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
	// Outcome is succeeded, failed, handed_back or lost; absent while the
	// attempt runs.
	Outcome   *string `json:"outcome,omitempty"`
	SessionID *string `json:"session_id,omitempty"`
	// RecoveredFrom is the earlier attempt whose backend session this one
	// continued; RecoverSkipped says why one that could have did not.
	RecoveredFrom  *int     `json:"recovered_from,omitempty"`
	RecoverSkipped *string  `json:"recover_skipped,omitempty"`
	CostUSD        *float64 `json:"cost_usd,omitempty"`
	SessionCostUSD *float64 `json:"session_cost_usd,omitempty"`
}

const attemptCols = `attempt, worker, runner_kind, spec_sha, started_at, ended_at, outcome, session_id,
	recovered_from, recover_skipped, cost_usd::float8, session_cost_usd::float8`

// ListAttempts returns a run's attempts, oldest first. A run claimed before
// attempts were recorded has none for those claims.
func (s *Store) ListAttempts(ctx context.Context, runID string) ([]Attempt, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+attemptCols+` FROM run_attempts WHERE run_id = $1 ORDER BY attempt`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Attempt])
}

// PriorAttempt returns the attempt before attempt, or nil when there is
// none or it was never recorded.
func (s *Store) PriorAttempt(ctx context.Context, runID string, attempt int) (*Attempt, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+attemptCols+` FROM run_attempts WHERE run_id = $1 AND attempt = $2`,
		runID, attempt-1)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Attempt])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

// AttachSession records the backend session a running attempt writes to.
// It is called as soon as the backend names it, before any outcome, so an
// attempt that is interrupted still says where its work is.
func (s *Store) AttachSession(ctx context.Context, runID string, attempt int, sessionID string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE run_attempts a SET session_id = $3
		FROM runs r
		WHERE a.run_id = $1 AND a.attempt = $2 AND a.outcome IS NULL
		  AND r.id = a.run_id AND r.status = 'running' AND r.attempt = a.attempt`, runID, attempt, sessionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("attach session to run %s attempt %d: %w (not its running claim)", runID, attempt, ErrConflict)
	}
	return nil
}

// SetRecovery records whether a running attempt continued an earlier
// attempt's session (from > 0) or why it did not (skipped).
func (s *Store) SetRecovery(ctx context.Context, runID string, attempt, from int, skipped string) error {
	var f *int
	if from > 0 {
		f = &from
	}
	_, err := s.Pool.Exec(ctx, `UPDATE run_attempts SET recovered_from = $3, recover_skipped = NULLIF($4, '')
		WHERE run_id = $1 AND attempt = $2 AND outcome IS NULL`, runID, attempt, f, skipped)
	return err
}

// SetAttemptCost records an ended attempt's spend when nothing recorded it
// yet: an attempt whose process died is costed by the next attempt, from
// what the backend saved in its transcript.
func (s *Store) SetAttemptCost(ctx context.Context, runID string, attempt int, cost, sessionCost *float64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE run_attempts SET cost_usd = $3, session_cost_usd = $4
		WHERE run_id = $1 AND attempt = $2 AND outcome IS NOT NULL AND cost_usd IS NULL`,
		runID, attempt, cost, sessionCost)
	return err
}

// StoredInput is one artifact's name and content.
type StoredInput struct {
	Name      string
	MediaType string
	Content   []byte
}

// AttemptArtifacts returns one attempt's artifacts in a direction ("in" or
// "out"), by name.
func (s *Store) AttemptArtifacts(ctx context.Context, runID string, attempt int, direction string) ([]StoredInput, error) {
	rows, err := s.Pool.Query(ctx, `SELECT name, media_type, content FROM artifacts
		WHERE run_id = $1 AND attempt = $2 AND direction = $3 ORDER BY name`, runID, attempt, direction)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[StoredInput])
}
