package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Polled subscription usage (usage_samples) and the scheduled runs held
// back while it is high (run_deferrals). internal/usage decides; this
// stores.

// UsageSample is one poll of the subscription usage endpoint.
type UsageSample struct {
	ID         int64           `json:"id"`
	PolledAt   time.Time       `json:"polled_at"`
	Worker     *string         `json:"worker"`
	OK         bool            `json:"ok"`
	HTTPStatus *int            `json:"http_status"`
	Error      *string         `json:"error"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// RecordUsageSample stores one poll: the body when ok, else the error.
func (s *Store) RecordUsageSample(ctx context.Context, worker string, httpStatus int, payload json.RawMessage, pollErr error) error {
	var status *int
	if httpStatus != 0 {
		status = &httpStatus
	}
	var msg *string
	if pollErr != nil {
		m := pollErr.Error()
		msg, payload = &m, nil
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO usage_samples (worker, ok, http_status, error, payload)
		VALUES (NULLIF($1, ''), $2, $3, $4, $5)`, worker, pollErr == nil, status, msg, nullJSON(payload))
	return err
}

// LatestUsageSamples returns the newest successful sample within
// RateLimitLookback and the newest sample of any outcome; either may be nil.
func (s *Store) LatestUsageSamples(ctx context.Context) (ok, last *UsageSample, err error) {
	read := func(where string, args ...any) (*UsageSample, error) {
		var u UsageSample
		err := s.Pool.QueryRow(ctx, `SELECT id, polled_at, worker, ok, http_status, error, payload
			FROM usage_samples `+where+` ORDER BY polled_at DESC, id DESC LIMIT 1`, args...).
			Scan(&u.ID, &u.PolledAt, &u.Worker, &u.OK, &u.HTTPStatus, &u.Error, &u.Payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return &u, err
	}
	if ok, err = read(`WHERE ok AND polled_at > now() - $1::interval`, RateLimitLookback.String()); err != nil {
		return nil, nil, err
	}
	last, err = read(``)
	return ok, last, err
}

// Deferral is why a run is held: the window over the threshold and the
// reading behind it.
type Deferral struct {
	Window      string
	Utilization float64
	Threshold   float64
	Source      string
	ObservedAt  time.Time
}

// HeldRun is a run held back now.
type HeldRun struct {
	RunID       string    `json:"run_id"`
	Agent       string    `json:"agent"`
	FirstAt     time.Time `json:"first_at"`
	LastAt      time.Time `json:"last_at"`
	Times       int       `json:"times"`
	Window      string    `json:"window"`
	Utilization float64   `json:"utilization"`
	Threshold   float64   `json:"threshold"`
	Source      string    `json:"source"`
	ObservedAt  time.Time `json:"observed_at"`
	HeldUntil   time.Time `json:"held_until"`
}

// AgentBudgetUsage is API-equivalent usage from runs that started an
// attempt in one agent's current budget period.
type AgentBudgetUsage struct {
	APIEquivalentUSD json.Number `json:"api_equivalent_usd"`
	Runs             int         `json:"runs"`
	RunsWithCost     int         `json:"runs_with_cost"`
}

// BudgetUsage totals known cost and runner-started runs for an agent in
// [start, end). A gate-skipped run did not reach a runner and does not count.
func (s *Store) BudgetUsage(ctx context.Context, agent string, start, end time.Time, excludeRunID string) (AgentBudgetUsage, error) {
	var out AgentBudgetUsage
	var cost string
	err := s.Pool.QueryRow(ctx, `SELECT coalesce(round(sum(cost_usd) FILTER (WHERE attempt > 0 AND status <> 'skipped'), 6), 0)::text,
		count(*) FILTER (WHERE attempt > 0 AND status <> 'skipped'),
		count(cost_usd) FILTER (WHERE attempt > 0 AND status <> 'skipped')
		FROM runs WHERE agent = $1 AND created_at >= $2 AND created_at < $3
		AND (NULLIF($4, '')::uuid IS NULL OR id <> NULLIF($4, '')::uuid)`, agent, start, end, excludeRunID).
		Scan(&cost, &out.Runs, &out.RunsWithCost)
	out.APIEquivalentUSD = json.Number(cost)
	return out, err
}

// DueScheduledRuns returns the queued schedule runs of agents that an
// executor may claim now, oldest first.
func (s *Store) DueScheduledRuns(ctx context.Context, agents []string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text FROM runs
		WHERE status = 'queued' AND trigger = 'schedule' AND agent = ANY ($1)
		  AND (run_after IS NULL OR run_after <= now())
		ORDER BY created_at`, agents)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// DeferRun holds a queued schedule run until until and records why, in one
// transaction. It reports false, writing nothing, when the run is no longer
// a queued schedule run (an executor claimed it first).
func (s *Store) DeferRun(ctx context.Context, runID string, until time.Time, d Deferral) (bool, error) {
	held := false
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var agent string
		err := tx.QueryRow(ctx, `UPDATE runs SET run_after = $2
			WHERE id = $1 AND status = 'queued' AND trigger = 'schedule' RETURNING agent`, runID, until).Scan(&agent)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		held = true
		_, err = tx.Exec(ctx, `
			INSERT INTO run_deferrals (run_id, agent, window_name, utilization, threshold, source, observed_at, held_until)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (run_id) DO UPDATE SET last_at = now(), times = run_deferrals.times + 1,
			    window_name = EXCLUDED.window_name, utilization = EXCLUDED.utilization,
			    threshold = EXCLUDED.threshold, source = EXCLUDED.source,
			    observed_at = EXCLUDED.observed_at, held_until = EXCLUDED.held_until, released_at = NULL`,
			runID, agent, d.Window, d.Utilization, d.Threshold, d.Source, d.ObservedAt, until)
		return err
	})
	return held, err
}

// ReleaseDeferrals lets go of every held run whose agent is not in keep:
// its run_after is cleared, so it is claimable at once, and its deferral
// records when. It returns how many it released.
func (s *Store) ReleaseDeferrals(ctx context.Context, keep []string) (int, error) {
	if keep == nil {
		keep = []string{}
	}
	var n int
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE runs r SET run_after = NULL FROM run_deferrals d
			WHERE d.run_id = r.id AND d.released_at IS NULL AND NOT (d.agent = ANY ($1))
			  AND r.status = 'queued' AND r.trigger = 'schedule'`, keep); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE run_deferrals SET released_at = now()
			WHERE released_at IS NULL AND NOT (agent = ANY ($1))`, keep)
		n = int(tag.RowsAffected())
		return err
	})
	return n, err
}

// HeldRuns lists the runs held now (queued, with a deferral not released),
// oldest hold first.
func (s *Store) HeldRuns(ctx context.Context) ([]HeldRun, error) {
	rows, err := s.Pool.Query(ctx, `SELECT d.run_id::text, d.agent, d.first_at, d.last_at, d.times, d.window_name,
		       d.utilization, d.threshold, d.source, d.observed_at, d.held_until
		FROM run_deferrals d JOIN runs r ON r.id = d.run_id
		WHERE d.released_at IS NULL AND r.status = 'queued'
		ORDER BY d.first_at, d.run_id LIMIT $1`, maxRunning)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[HeldRun])
}
