package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Hold kinds: what the backend refused.
const (
	HoldAuth      = "auth"
	HoldRateLimit = "rate_limit"
)

// holdFilter keeps a claim off a held runner's unattended runs: the agent's current spec names the runner (applyDefaults fills it), and backend_holds_one_open has an open row for it. A person's own run (a message, a manual run) is never held; it is how a person tells orbit the backend is back (CloseHold).
const holdFilter = `
	AND (q.trigger IN ('message', 'manual') OR NOT EXISTS (
	    SELECT 1 FROM backend_holds h
	    JOIN agents ha ON ha.name = q.agent
	    JOIN agent_specs hs ON hs.sha = ha.spec_sha
	    WHERE h.closed_at IS NULL AND h.runner = hs.spec->>'runner'))`

// Hold is one backend hold.
type Hold struct {
	ID             string     `json:"id"`
	Runner         string     `json:"runner"`
	Kind           string     `json:"kind"`
	Reason         string     `json:"reason"`
	RunID          *string    `json:"run_id"`
	OpenedAt       time.Time  `json:"opened_at"`
	ResetsAt       *time.Time `json:"resets_at"`
	NextProbeAt    time.Time  `json:"next_probe_at"`
	Probes         int        `json:"probes"`
	LastProbeAt    *time.Time `json:"last_probe_at"`
	LastProbeError *string    `json:"last_probe_error"`
	ClosedAt       *time.Time `json:"closed_at,omitempty"`
	ClosedBy       *string    `json:"closed_by,omitempty"`
}

const holdCols = `id::text, runner, kind, reason, run_id::text, opened_at, resets_at, next_probe_at, probes,
	last_probe_at, last_probe_error, closed_at, closed_by`

func scanHold(row pgx.Row) (*Hold, error) {
	var h Hold
	err := row.Scan(&h.ID, &h.Runner, &h.Kind, &h.Reason, &h.RunID, &h.OpenedAt, &h.ResetsAt, &h.NextProbeAt, &h.Probes,
		&h.LastProbeAt, &h.LastProbeError, &h.ClosedAt, &h.ClosedBy)
	return &h, err
}

// NewHold is what opening a hold needs. FirstProbe is when the backend is first tried again.
type NewHold struct {
	Runner, Kind, Reason, RunID string
	ResetsAt                    *time.Time
	FirstProbe                  time.Time
}

// OpenHold holds a runner. It returns opened false when the runner was held already (backend_holds_one_open): the run that found out is one more failure of the same cause, and nothing new is recorded.
func (s *Store) OpenHold(ctx context.Context, h NewHold) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO backend_holds (runner, kind, reason, run_id, resets_at, next_probe_at)
		VALUES ($1, $2, $3, NULLIF($4, '')::uuid, $5, $6)
		ON CONFLICT (runner) WHERE closed_at IS NULL DO NOTHING`,
		h.Runner, h.Kind, h.Reason, h.RunID, h.ResetsAt, h.FirstProbe)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// OpenHolds lists the runners held now, by runner.
func (s *Store) OpenHolds(ctx context.Context) ([]Hold, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+holdCols+` FROM backend_holds WHERE closed_at IS NULL ORDER BY runner`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Hold{}
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

// ClaimProbe takes one open hold whose probe is due and moves its next probe to retry later, so one instance probes it once per interval whatever the number of instances. It returns nil when no probe is due.
func (s *Store) ClaimProbe(ctx context.Context, retry time.Duration) (*Hold, error) {
	h, err := scanHold(s.Pool.QueryRow(ctx, `
		UPDATE backend_holds SET next_probe_at = now() + $1::interval, probes = probes + 1, last_probe_at = now()
		WHERE id = (SELECT id FROM backend_holds WHERE closed_at IS NULL AND next_probe_at <= now()
		            ORDER BY next_probe_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+holdCols, retry.String()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return h, err
}

// RecordProbe records a probe's result: success closes the hold (closed_by probe), a failure is kept as its last error.
func (s *Store) RecordProbe(ctx context.Context, id string, probeErr error) error {
	if probeErr == nil {
		_, err := s.Pool.Exec(ctx, `UPDATE backend_holds SET closed_at = now(), closed_by = 'probe', last_probe_error = NULL
			WHERE id = $1 AND closed_at IS NULL`, id)
		return err
	}
	_, err := s.Pool.Exec(ctx, `UPDATE backend_holds SET last_probe_error = $2 WHERE id = $1 AND closed_at IS NULL`, id, probeErr.Error())
	return err
}

// CloseHold closes a runner's open hold, by names who did (run:<id> for a person's run that succeeded). It reports whether one was open.
func (s *Store) CloseHold(ctx context.Context, runner, by string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE backend_holds SET closed_at = now(), closed_by = $2
		WHERE runner = $1 AND closed_at IS NULL`, runner, by)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// AttemptRateLimitEvents are the rate_limit_event lines one attempt's stream carried, oldest first.
func (s *Store) AttemptRateLimitEvents(ctx context.Context, runID string, attempt int) ([]RateLimitEvent, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT run_id::text, at, payload FROM run_events
		WHERE run_id = $1 AND attempt = $2 AND kind = 'rate_limit_event' ORDER BY seq`, runID, attempt)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[RateLimitEvent])
}
