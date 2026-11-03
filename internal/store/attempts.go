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
	// Outcome is succeeded, failed, skipped, handed_back or lost; absent while the
	// attempt runs.
	Outcome   *string `json:"outcome,omitempty"`
	SessionID *string `json:"session_id,omitempty"`
	// RecoveredFrom is the earlier attempt whose backend session this one
	// continued; RecoverSkipped says why one that could have did not.
	RecoveredFrom  *int     `json:"recovered_from,omitempty"`
	RecoverSkipped *string  `json:"recover_skipped,omitempty"`
	CostUSD        *float64 `json:"cost_usd,omitempty"`
	SessionCostUSD *float64 `json:"session_cost_usd,omitempty"`
	// Tokens is what the attempt's backend calls spent in tokens; Prefix the stable prefix it sent.
	Tokens *Tokens        `json:"tokens,omitempty"`
	Prefix *AttemptPrefix `json:"prefix,omitempty"`
}

var attemptCols = `attempt, worker, runner_kind, spec_sha, started_at, ended_at, outcome, session_id,
	recovered_from, recover_skipped, cost_usd::float8, session_cost_usd::float8, ` + attemptTokens + `, ` + attemptPrefix

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

// AttemptChain is the narration an attempt continues: the earlier attempts
// whose sessions it resumed, transitively, oldest first, ending with
// attempt itself. settled is false while a just-claimed attempt has not yet
// decided whether it continues the one before (planRecovery records that
// before its first event); an attempt 1, one with events, one without a row (claimed by a
// version that recorded none), or one whose run is final is settled.
func (s *Store) AttemptChain(ctx context.Context, runID string, attempt int) (chain []int, settled bool, err error) {
	if attempt < 1 {
		return nil, true, nil
	}
	// One snapshot: a SetRecovery between the two reads would give the
	// chain before it with settled after it, and a stream would start a new
	// narration for an attempt that continues the last.
	err = pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH RECURSIVE c(attempt, recovered_from) AS (
			    SELECT attempt, recovered_from FROM run_attempts WHERE run_id = $1 AND attempt = $2
			    UNION ALL
			    SELECT a.attempt, a.recovered_from FROM run_attempts a JOIN c ON a.run_id = $1 AND a.attempt = c.recovered_from
			)
			SELECT attempt FROM c ORDER BY attempt`, runID, attempt)
		if err != nil {
			return err
		}
		if chain, err = pgx.CollectRows(rows, pgx.RowTo[int]); err != nil {
			return err
		}
		if len(chain) == 0 {
			chain, settled = []int{attempt}, true
			return nil
		}
		return tx.QueryRow(ctx, `
			SELECT $2 = 1 OR a.recovered_from IS NOT NULL OR a.recover_skipped IS NOT NULL
			       OR r.status IN ('succeeded', 'failed', 'cancelled', 'skipped')
			       OR EXISTS (SELECT 1 FROM run_events e WHERE e.run_id = $1 AND e.attempt = $2)
			FROM run_attempts a JOIN runs r ON r.id = a.run_id
			WHERE a.run_id = $1 AND a.attempt = $2`, runID, attempt).Scan(&settled)
	})
	if err != nil {
		return nil, false, err
	}
	return chain, settled, nil
}

// ToolCallRunning reports whether a tool call of the attempt is still
// running by its journal: a tool_call item whose newest revision's state is
// running. The live journal write is in AppendEvent's transaction, so this
// is as current as the transcript. The stall watchdog reads it, since a
// tool may run silently for as long as it takes.
func (s *Store) ToolCallRunning(ctx context.Context, runID string, attempt int) (bool, error) {
	var running bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM (
		    SELECT DISTINCT ON (item_id) body->>'state' AS state FROM journal_items
		    WHERE run_id = $1 AND attempt = $2 AND kind = 'tool_call'
		    ORDER BY item_id, revision DESC) t
		WHERE t.state = 'running')`, runID, attempt).Scan(&running)
	return running, err
}

// AttemptStalled reports whether the attempt's turn was ended by the stall
// watchdog (an orbit.stalled event), so the attempt that continues it says
// why it was interrupted.
func (s *Store) AttemptStalled(ctx context.Context, runID string, attempt int) (bool, error) {
	var stalled bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_events WHERE run_id = $1 AND attempt = $2 AND kind = 'orbit.stalled')`,
		runID, attempt).Scan(&stalled)
	return stalled, err
}
