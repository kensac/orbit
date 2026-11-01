package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---------------------------------------------------------------- sessions

// SetSession records how a running attempt uses its backend session. It is
// written before the backend is called, so an attempt that is interrupted
// still names the session it touched (see LatestSession's Tainted).
func (s *Store) SetSession(ctx context.Context, runID, mode, parent string, depth int) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE runs SET session_mode = $2, session_parent = NULLIF($3, ''), session_depth = $4
		WHERE id = $1 AND status = 'running'`, runID, mode, parent, depth)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set session of run %s: %w (not running)", runID, ErrConflict)
	}
	return nil
}

// PriorSession is the session an agent's latest finished run left.
type PriorSession struct {
	RunID      string
	Status     string
	SpecSHA    string
	RunnerKind string
	SessionID  string
	FinishedAt time.Time
	// Depth is how many runs the session's chain holds.
	Depth int
	// CostUSD is the backend's cumulative total after that run.
	CostUSD *float64
	// Tainted is true when a live turn on this session did not succeed:
	// the live process appended that turn to the session's transcript, so
	// continuing it would carry the failed turn along.
	Tainted bool
}

// LatestSession returns the latest finished (succeeded or failed) run of
// one thread of the agent (ticketID's, or the general thread when nil) and
// its session, or nil if it has none. A ticket never continues another
// thread's session. Whether to continue it is the engine's policy; this
// only reports.
func (s *Store) LatestSession(ctx context.Context, agent string, ticketID *string) (*PriorSession, error) {
	var p PriorSession
	var kind, sid *string
	var depth *int
	err := s.Pool.QueryRow(ctx, `
		SELECT r.id::text, r.status, r.spec_sha, r.runner_kind, r.session_id, r.finished_at,
		       r.session_depth, r.session_cost_usd::float8,
		       r.session_id IS NOT NULL AND EXISTS (
		           SELECT 1 FROM runs t
		           WHERE t.agent = r.agent AND t.session_mode = 'live'
		             AND t.session_parent = r.session_id AND t.status <> 'succeeded')
		FROM runs r
		WHERE r.agent = $1 AND r.status IN ('succeeded', 'failed')
		  AND r.ticket_id IS NOT DISTINCT FROM $2::uuid
		ORDER BY r.finished_at DESC LIMIT 1`, agent, ticketID).
		Scan(&p.RunID, &p.Status, &p.SpecSHA, &kind, &sid, &p.FinishedAt, &depth, &p.CostUSD, &p.Tainted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if kind != nil {
		p.RunnerKind = *kind
	}
	if sid != nil {
		p.SessionID = *sid
	}
	if depth != nil {
		p.Depth = *depth
	}
	return &p, nil
}

// ---------------------------------------------------------------- continuations

// ErrChainLimit is returned when a continuation would make a chain longer
// than the caller allows.
var ErrChainLimit = errors.New("continuation chain limit reached")

// Continuation is a wake an agent asked for.
type Continuation struct {
	Agent   string
	FromRun string
	After   time.Duration
	Note    string
	// MaxChain bounds continuations in a row.
	MaxChain int
}

// EnqueueContinuation queues a continue run due After from now. The chain
// is one longer than FromRun's when FromRun was itself a continuation, else
// 1. runs_one_continuation keeps one waiting per agent: when one already
// waits, its id is returned with queued false and nothing is written. The
// continuation stays in FromRun's thread (its ticket_id).
func (s *Store) EnqueueContinuation(ctx context.Context, c Continuation) (id string, queued bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var trigger string
		var chain int
		if err := tx.QueryRow(ctx, `SELECT trigger, chain FROM runs WHERE id = $1 AND agent = $2`, c.FromRun, c.Agent).
			Scan(&trigger, &chain); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		next := 1
		if trigger == "continue" {
			next = chain + 1
		}
		if next > c.MaxChain {
			return fmt.Errorf("%w: this would be continuation %d of at most %d", ErrChainLimit, next, c.MaxChain)
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO runs (agent, spec_sha, trigger, prompt, run_after, continues, chain, ticket_id)
			SELECT a.name, a.spec_sha, 'continue', $2, now() + $3::interval, $4, $5, f.ticket_id
			FROM agents a, runs f WHERE a.name = $1 AND f.id = $4
			ON CONFLICT (agent) WHERE trigger = 'continue' AND status = 'queued' DO NOTHING
			RETURNING id::text`, c.Agent, c.Note, c.After.String(), c.FromRun, next).Scan(&id)
		if err == nil {
			queued = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return tx.QueryRow(ctx, `SELECT id::text FROM runs WHERE agent = $1 AND trigger = 'continue' AND status = 'queued'`,
			c.Agent).Scan(&id)
	})
	return id, queued, err
}
