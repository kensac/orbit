package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// ---------------------------------------------------------------- claims

// Claim says which runs an executor may take.
type Claim struct {
	Worker string
	// Any claims a run whatever its agent needs (a host advertising "*").
	Any bool
	// Capabilities are the host's: a run is claimed only when its agent's current spec needs a subset of them.
	Capabilities []string
	// Builds lets the executor claim build runs, which need the GitHub App and sandboxd: control instances only.
	Builds bool
}

// ClaimRunFor is ClaimRun limited to what one executor may run: the oldest queued run whose agent has nothing running, whose agent's current spec needs only capabilities in c, and, unless c.Builds, that is not a build. Needs are read from the spec JSON (`needs`), so an agent defined by any writer of agent_specs is covered, and an agent with no needs is claimed anywhere.
func (s *Store) ClaimRunFor(ctx context.Context, c Claim) (*ClaimedRun, error) {
	caps := c.Capabilities
	if caps == nil {
		caps = []string{}
	}
	capsJSON, err := json.Marshal(caps)
	if err != nil {
		return nil, err
	}
	return s.claim(ctx, c.Worker, `
		AND ($2::bool OR COALESCE((
		    SELECT sp.spec->'needs' FROM agents a JOIN agent_specs sp ON sp.sha = a.spec_sha
		    WHERE a.name = q.agent), '[]'::jsonb) <@ $3::jsonb)
		AND ($4::bool OR q.trigger <> 'build')`, c.Any, string(capsJSON), c.Builds)
}

// ---------------------------------------------------------------- config

// SyncDeployment records the deployment control loaded and makes it the one in force. Every version is kept.
func (s *Store) SyncDeployment(ctx context.Context, sha string, doc []byte) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO deployment_specs (sha, spec) VALUES ($1, $2)
			ON CONFLICT (sha) DO NOTHING`, sha, doc); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO deployment_current (id, sha) VALUES (true, $1)
			ON CONFLICT (id) DO UPDATE SET sha = EXCLUDED.sha,
			    updated_at = CASE WHEN deployment_current.sha <> EXCLUDED.sha THEN now() ELSE deployment_current.updated_at END`, sha)
		return err
	})
}

// StoredAgent is one present agent's current spec as recorded.
type StoredAgent struct {
	Name string
	SHA  string
	JSON []byte
}

// StoredConfig returns the deployment in force and every present agent's current spec, what a worker builds its configuration from. ErrNotFound means control has not recorded a deployment yet.
func (s *Store) StoredConfig(ctx context.Context) (deployment []byte, agents []StoredAgent, err error) {
	err = pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT d.spec FROM deployment_current c JOIN deployment_specs d ON d.sha = c.sha`).
			Scan(&deployment); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		rows, err := tx.Query(ctx, `SELECT a.name, a.spec_sha, sp.spec FROM agents a
			JOIN agent_specs sp ON sp.sha = a.spec_sha WHERE a.present ORDER BY a.name`)
		if err != nil {
			return err
		}
		agents, err = pgx.CollectRows(rows, pgx.RowToStructByPos[StoredAgent])
		return err
	})
	return deployment, agents, err
}

// SchemaCurrent reports whether every migration this binary embeds is applied. A worker never migrates (control does, at start); it waits rather than run against a schema older than its code.
func (s *Store) SchemaCurrent(ctx context.Context) (bool, error) {
	db := stdlib.OpenDBFromPool(s.Pool)
	defer db.Close()
	p, err := newProvider(db)
	if err != nil {
		return false, err
	}
	pending, err := p.HasPending(ctx)
	return !pending, err
}

// ---------------------------------------------------------------- outbox

// OutboxItem is a notification a worker recorded for control to send.
type OutboxItem struct {
	ID       int64
	Agent    string
	RunID    *string
	ActionID *string
	Purpose  string
	Message  json.RawMessage
}

// Enqueue records a notification for control to send.
func (s *Store) Enqueue(ctx context.Context, worker string, it OutboxItem) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO outbox (agent, run_id, action_id, purpose, message, worker)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`, it.Agent, it.RunID, it.ActionID, it.Purpose, it.Message, worker)
	return err
}

// OutboxReclaimAfter is how long a claimed, unsent notification waits before another control instance may take it: the claimer died mid-send. Above a send's one-minute timeout per channel (engine.deliver) for a few channels.
const OutboxReclaimAfter = 5 * time.Minute

// ClaimOutbox takes the oldest unsent notification, or returns nil.
func (s *Store) ClaimOutbox(ctx context.Context) (*OutboxItem, error) {
	var it OutboxItem
	err := s.Pool.QueryRow(ctx, `
		UPDATE outbox SET claimed_at = now()
		WHERE id = (SELECT id FROM outbox
		            WHERE sent_at IS NULL AND (claimed_at IS NULL OR claimed_at < now() - $1::interval)
		            ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id, agent, run_id::text, action_id::text, purpose, message`, OutboxReclaimAfter.String()).
		Scan(&it.ID, &it.Agent, &it.RunID, &it.ActionID, &it.Purpose, &it.Message)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// OutboxSent marks a notification handled; its deliveries rows say how each channel fared.
func (s *Store) OutboxSent(ctx context.Context, id int64) error {
	_, err := s.Pool.Exec(ctx, `UPDATE outbox SET sent_at = now() WHERE id = $1 AND sent_at IS NULL`, id)
	return err
}

// ---------------------------------------------------------------- sessions

// SessionTranscript is one stored Claude Code session.
type SessionTranscript struct {
	SessionID string
	Agent     string
	RunID     string
	Attempt   int
	Worker    string
	// Archive is the gzipped tar (engine/sessionstore.go); Bytes is its uncompressed size.
	Archive []byte
	Bytes   int64
}

// PutSessionTranscript stores a session's transcript. A session id is stored once: a resume forks to a new id, so a later write of the same id carries nothing new.
func (s *Store) PutSessionTranscript(ctx context.Context, t SessionTranscript) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO session_transcripts (session_id, agent, run_id, attempt, worker, archive, bytes)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7) ON CONFLICT (session_id) DO NOTHING`,
		t.SessionID, t.Agent, t.RunID, t.Attempt, t.Worker, t.Archive, t.Bytes)
	if isForeignKeyViolation(err) {
		return fmt.Errorf("store session %s: %w", t.SessionID, ErrNotFound)
	}
	return err
}

// GetSessionTranscript returns a stored session's archive, or ErrNotFound.
func (s *Store) GetSessionTranscript(ctx context.Context, sessionID string) ([]byte, error) {
	var b []byte
	err := s.Pool.QueryRow(ctx, `SELECT archive FROM session_transcripts WHERE session_id = $1`, sessionID).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}
