// Package store is orbit's only access to Postgres. Every write the harness
// makes goes through a method here, so the transaction boundaries and the
// invariants the schema cannot express live in one file.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a state transition is no longer possible, for
// example approving an action someone else already decided.
var ErrConflict = errors.New("conflict")

// Store wraps the pool.
type Store struct {
	Pool *pgxpool.Pool
}

// Open connects to url.
func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.Pool.Close() }

// Migrate applies every embedded migration.
func (s *Store) Migrate(ctx context.Context) error {
	db := stdlib.OpenDBFromPool(s.Pool)
	defer db.Close()
	return migrate(ctx, db)
}

func migrate(ctx context.Context, db *sql.DB) error {
	p, err := goose.NewProvider(goose.DialectPostgres, db, mustSub(migrations, "migrations"))
	if err != nil {
		return err
	}
	_, err = p.Up(ctx)
	return err
}

// ---------------------------------------------------------------- agents

// SyncAgents records the loaded configuration: every spec version, each
// agent's current spec, and present = false for agents no longer defined.
// specs maps agent name to (sha, canonical JSON, enabled).
func (s *Store) SyncAgents(ctx context.Context, specs map[string]AgentSpec) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		names := make([]string, 0, len(specs))
		for name, a := range specs {
			names = append(names, name)
			if _, err := tx.Exec(ctx, `
				INSERT INTO agent_specs (sha, agent, spec) VALUES ($1, $2, $3)
				ON CONFLICT (sha) DO NOTHING`, a.SHA, name, a.JSON); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO agents (name, spec_sha, enabled) VALUES ($1, $2, $3)
				ON CONFLICT (name) DO UPDATE SET
				    spec_sha = EXCLUDED.spec_sha, enabled = EXCLUDED.enabled, present = true,
				    updated_at = CASE WHEN agents.spec_sha <> EXCLUDED.spec_sha
				                        OR agents.enabled <> EXCLUDED.enabled
				                        OR NOT agents.present
				                      THEN now() ELSE agents.updated_at END`,
				name, a.SHA, a.Enabled); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `
			UPDATE agents SET present = false, updated_at = now()
			WHERE present AND NOT (name = ANY($1))`, names)
		return err
	})
}

// AgentSpec is one agent's loaded spec, as SyncAgents records it.
type AgentSpec struct {
	SHA     string
	JSON    []byte
	Enabled bool
}

// AgentRow is an agent as the API reports it.
type AgentRow struct {
	Name      string          `json:"name"`
	SpecSHA   string          `json:"spec_sha"`
	Spec      json.RawMessage `json:"spec"`
	Present   bool            `json:"present"`
	Enabled   bool            `json:"enabled"`
	FirstSeen time.Time       `json:"first_seen"`
	UpdatedAt time.Time       `json:"updated_at"`
	LastRun   *RunSummary     `json:"last_run,omitempty"`
	Unread    int             `json:"unread_messages"`
	Pending   int             `json:"pending_actions"`
}

// RunSummary is the short form of a run.
type RunSummary struct {
	ID         string     `json:"id"`
	Status     string     `json:"status"`
	Trigger    string     `json:"trigger"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Summary    *string    `json:"summary,omitempty"`
}

// ListAgents returns every agent ever defined, current ones first.
func (s *Store) ListAgents(ctx context.Context) ([]AgentRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT a.name, a.spec_sha, sp.spec, a.present, a.enabled, a.first_seen, a.updated_at,
		       lr.id, lr.status, lr.trigger, lr.created_at, lr.finished_at, lr.summary,
		       (SELECT count(*) FROM messages m
		         WHERE m.agent = a.name AND `+openQuestion+`),
		       (SELECT count(*) FROM actions x WHERE x.agent = a.name AND x.status = 'pending')
		FROM agents a
		JOIN agent_specs sp ON sp.sha = a.spec_sha
		LEFT JOIN LATERAL (
		    SELECT r.id::text, r.status, r.trigger, r.created_at, r.finished_at, r.summary
		    FROM runs r WHERE r.agent = a.name ORDER BY r.created_at DESC LIMIT 1
		) lr ON true
		ORDER BY a.present DESC, a.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentRow
	for rows.Next() {
		var a AgentRow
		var id, status, trigger *string
		var created *time.Time
		var finished *time.Time
		var summary *string
		if err := rows.Scan(&a.Name, &a.SpecSHA, &a.Spec, &a.Present, &a.Enabled, &a.FirstSeen, &a.UpdatedAt,
			&id, &status, &trigger, &created, &finished, &summary, &a.Unread, &a.Pending); err != nil {
			return nil, err
		}
		if id != nil {
			a.LastRun = &RunSummary{ID: *id, Status: *status, Trigger: *trigger, CreatedAt: *created, FinishedAt: finished, Summary: summary}
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- runs

// NewRun is what enqueueing a run needs.
type NewRun struct {
	Agent        string
	SpecSHA      string
	Trigger      string
	TriggerIndex *int
	ScheduledFor *time.Time
	Prompt       string
}

// EnqueueRun inserts a queued run. For a schedule fire already recorded it
// returns ("", nil): the fire was handled before, by this process or another.
func (s *Store) EnqueueRun(ctx context.Context, r NewRun) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO runs (agent, spec_sha, trigger, trigger_index, scheduled_for, prompt)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (agent, trigger_index, scheduled_for) WHERE trigger = 'schedule' DO NOTHING
		RETURNING id::text`,
		r.Agent, r.SpecSHA, r.Trigger, r.TriggerIndex, r.ScheduledFor, r.Prompt).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// LastScheduledFire returns the newest scheduled_for recorded for one trigger,
// or nil if it never fired.
func (s *Store) LastScheduledFire(ctx context.Context, agent string, triggerIndex int) (*time.Time, error) {
	var t *time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT max(scheduled_for) FROM runs
		WHERE agent = $1 AND trigger = 'schedule' AND trigger_index = $2`, agent, triggerIndex).Scan(&t)
	return t, err
}

// ClaimedRun is a run the executor now owns.
type ClaimedRun struct {
	ID      string
	Attempt int
	Agent   string
	SpecSHA string
	Trigger string
	Prompt  string
	// Chain is how many continuations in a row this run is (0 unless its
	// trigger is continue).
	Chain int
}

// MaxAttempts bounds how often an interrupted run is claimed again. A run
// that kills its own worker (out of memory) would otherwise loop forever.
const MaxAttempts = 3

// ClaimRun moves the oldest queued run whose agent has nothing running to
// running, owned by worker. It returns nil when there is nothing to claim.
// runs_one_running is what makes "nothing running" true under concurrency: a
// second claimer for the same agent fails the index and tries the next row.
func (s *Store) ClaimRun(ctx context.Context, worker string) (*ClaimedRun, error) {
	for attempt := 0; attempt < 5; attempt++ {
		var c ClaimedRun
		err := s.Pool.QueryRow(ctx, `
			UPDATE runs SET status = 'running', worker = $1, started_at = now(), heartbeat_at = now(),
			       attempt = attempt + 1
			WHERE id = (
			    SELECT q.id FROM runs q
			    WHERE q.status = 'queued'
			      AND (q.run_after IS NULL OR q.run_after <= now())
			      AND NOT EXISTS (SELECT 1 FROM runs b WHERE b.agent = q.agent AND b.status = 'running')
			    ORDER BY q.created_at
			    LIMIT 1
			    FOR UPDATE SKIP LOCKED
			)
			RETURNING id::text, attempt, agent, spec_sha, trigger, prompt, chain`, worker).
			Scan(&c.ID, &c.Attempt, &c.Agent, &c.SpecSHA, &c.Trigger, &c.Prompt, &c.Chain)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if isUniqueViolation(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &c, nil
	}
	return nil, nil
}

// Heartbeat marks a running run alive.
func (s *Store) Heartbeat(ctx context.Context, runID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE runs SET heartbeat_at = now() WHERE id = $1 AND status = 'running'`, runID)
	return err
}

// ReapStale handles running runs whose heartbeat is older than staleAfter:
// their worker died. Each goes back to the queue, or fails once it has had
// MaxAttempts claims. It returns the ids it failed.
func (s *Store) ReapStale(ctx context.Context, staleAfter time.Duration) ([]string, error) {
	var failed []string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id::text, attempt FROM runs
			WHERE status = 'running' AND heartbeat_at < now() - $1::interval
			FOR UPDATE SKIP LOCKED`, staleAfter.String())
		if err != nil {
			return err
		}
		type stale struct {
			ID      string
			Attempt int
		}
		list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[stale])
		if err != nil {
			return err
		}
		for _, r := range list {
			if r.Attempt < MaxAttempts {
				if err := requeue(ctx, tx, r.ID); err != nil {
					return err
				}
				continue
			}
			if err := finishRun(ctx, tx, r.ID, RunResult{Status: "failed",
				Error: fmt.Sprintf("worker lost on attempt %d of %d", r.Attempt, MaxAttempts)}); err != nil {
				return err
			}
			if err := releaseMessages(ctx, tx, r.ID); err != nil {
				return err
			}
			failed = append(failed, r.ID)
		}
		return nil
	})
	return failed, err
}

// RequeueRun returns a running run to the queue: its process is shutting
// down. Nothing a run does outside its own rows happens before it finishes
// (actions and notifications follow RecordOutcome), so a fresh attempt is
// safe.
func (s *Store) RequeueRun(ctx context.Context, runID string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error { return requeue(ctx, tx, runID) })
}

func requeue(ctx context.Context, tx pgx.Tx, runID string) error {
	if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'queued', worker = NULL, started_at = NULL, heartbeat_at = NULL
		WHERE id = $1 AND status = 'running'`, runID); err != nil {
		return err
	}
	return releaseMessages(ctx, tx, runID)
}

// releaseMessages hands a run's consumed messages back to the inbox, so the
// next run reads what this one did not finish with. messages.json in the
// run's artifacts keeps the record that this attempt saw them.
func releaseMessages(ctx context.Context, db execer, runID string) error {
	_, err := db.Exec(ctx, `UPDATE messages SET run_id = NULL WHERE run_id = $1 AND direction = 'to_agent'`, runID)
	return err
}

// SetRunner records the backend and the spec a run executes under. The spec
// can differ from the one it was enqueued with when configuration reloaded
// while it waited; the row names the one it actually ran.
func (s *Store) SetRunner(ctx context.Context, runID, kind, model, specSHA string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE runs SET runner_kind = $2, model = NULLIF($3, ''), spec_sha = $4 WHERE id = $1`,
		runID, kind, model, specSHA)
	return err
}

// RunResult is a finished run's outcome.
type RunResult struct {
	Status    string
	Error     string
	Summary   string
	Report    string
	Output    json.RawMessage
	Usage     json.RawMessage
	CostUSD   *float64
	SessionID string
	// SessionCostUSD is the backend's cumulative total for the session.
	SessionCostUSD *float64
}

// FinishRun records the outcome of a run that wrote nothing else (a failure,
// or a success with no memory, questions or actions).
func (s *Store) FinishRun(ctx context.Context, runID string, r RunResult) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := finishRun(ctx, tx, runID, r); err != nil {
			return err
		}
		if r.Status != "succeeded" {
			return releaseMessages(ctx, tx, runID)
		}
		return nil
	})
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// finishRun is the one write of a run's final state. Only a running run can
// finish, so a run the reaper already failed stays failed.
func finishRun(ctx context.Context, db execer, runID string, r RunResult) error {
	tag, err := db.Exec(ctx, `
		UPDATE runs SET status = $2, finished_at = now(), error = NULLIF($3, ''),
		       summary = NULLIF($4, ''), report = NULLIF($5, ''), output = $6, usage = $7,
		       cost_usd = $8, session_id = NULLIF($9, ''), session_cost_usd = $10
		WHERE id = $1 AND status = 'running'`,
		runID, r.Status, r.Error, r.Summary, r.Report, nullJSON(r.Output), nullJSON(r.Usage), r.CostUSD, r.SessionID,
		r.SessionCostUSD)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finish run %s: %w (no longer running)", runID, ErrConflict)
	}
	return nil
}

// Run is a run as the API reports it.
type Run struct {
	ID           string          `json:"id"`
	Attempt      int             `json:"attempt"`
	Agent        string          `json:"agent"`
	SpecSHA      string          `json:"spec_sha"`
	Trigger      string          `json:"trigger"`
	TriggerIndex *int            `json:"trigger_index,omitempty"`
	ScheduledFor *time.Time      `json:"scheduled_for,omitempty"`
	Prompt       string          `json:"prompt"`
	Status       string          `json:"status"`
	RunnerKind   *string         `json:"runner_kind,omitempty"`
	Model        *string         `json:"model,omitempty"`
	Worker       *string         `json:"worker,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	Error        *string         `json:"error,omitempty"`
	Summary      *string         `json:"summary,omitempty"`
	Report       *string         `json:"report,omitempty"`
	Output       json.RawMessage `json:"output,omitempty"`
	Usage        json.RawMessage `json:"usage,omitempty"`
	CostUSD      *float64        `json:"cost_usd,omitempty"`
	SessionID    *string         `json:"session_id,omitempty"`
	// SessionMode is fresh, resumed or live for an agent with a session.
	SessionMode   *string    `json:"session_mode,omitempty"`
	SessionParent *string    `json:"session_parent,omitempty"`
	SessionDepth  *int       `json:"session_depth,omitempty"`
	RunAfter      *time.Time `json:"run_after,omitempty"`
	Continues     *string    `json:"continues,omitempty"`
	Chain         int        `json:"chain,omitempty"`
}

const runCols = `id::text, attempt, agent, spec_sha, trigger, trigger_index, scheduled_for, prompt, status,
	runner_kind, model, worker, created_at, started_at, finished_at, error, summary, report,
	output, usage, cost_usd::float8, session_id, session_mode, session_parent, session_depth,
	run_after, continues::text, chain`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Attempt, &r.Agent, &r.SpecSHA, &r.Trigger, &r.TriggerIndex, &r.ScheduledFor, &r.Prompt,
		&r.Status, &r.RunnerKind, &r.Model, &r.Worker, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.Error,
		&r.Summary, &r.Report, &r.Output, &r.Usage, &r.CostUSD, &r.SessionID, &r.SessionMode, &r.SessionParent,
		&r.SessionDepth, &r.RunAfter, &r.Continues, &r.Chain)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &r, err
}

// GetRun returns one run.
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.Pool.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, id))
}

// RunFilter narrows ListRuns. Before pages backwards through created_at.
type RunFilter struct {
	Agent  string
	Status string
	Before *time.Time
	Limit  int
}

// ListRuns returns runs newest first.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+runCols+` FROM runs
		WHERE ($1 = '' OR agent = $1) AND ($2 = '' OR status = $2)
		  AND ($3::timestamptz IS NULL OR created_at < $3)
		ORDER BY created_at DESC LIMIT $4`, f.Agent, f.Status, f.Before, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// CancelRun cancels a queued run. A running run cannot be cancelled here; the
// executor owns it until its timeout.
func (s *Store) CancelRun(ctx context.Context, id string) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE runs SET status = 'cancelled', finished_at = now()
		WHERE id = $1 AND status = 'queued'`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

// ---------------------------------------------------------------- events

// Event is one entry in a run's transcript.
type Event struct {
	Attempt int             `json:"attempt"`
	Seq     int             `json:"seq"`
	At      time.Time       `json:"at"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// AppendEvent stores the next transcript entry. Payloads that are not valid
// JSON are stored as a JSON string, so a malformed line from a runner is
// kept rather than dropped.
func (s *Store) AppendEvent(ctx context.Context, runID string, attempt, seq int, kind string, payload []byte) error {
	if !json.Valid(payload) {
		payload, _ = json.Marshal(string(payload))
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO run_events (run_id, attempt, seq, kind, payload) VALUES ($1, $2, $3, $4, $5)`,
		runID, attempt, seq, kind, payload)
	return err
}

// ListEvents returns one attempt's transcript from seq after. Attempt 0 means
// the run's latest attempt.
func (s *Store) ListEvents(ctx context.Context, runID string, attempt, after, limit int) ([]Event, error) {
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	rows, err := s.Pool.Query(ctx, `SELECT attempt, seq, at, kind, payload FROM run_events
		WHERE run_id = $1
		  AND attempt = CASE WHEN $2 > 0 THEN $2 ELSE (SELECT attempt FROM runs WHERE id = $1) END
		  AND seq > $3
		ORDER BY seq LIMIT $4`, runID, attempt, after, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Event])
}

// ---------------------------------------------------------------- artifacts

// Artifact is a stored input or output, without its content.
type Artifact struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Attempt   int       `json:"attempt"`
	Direction string    `json:"direction"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	SHA256    string    `json:"sha256"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// PutArtifact stores content for a run.
func (s *Store) PutArtifact(ctx context.Context, runID string, attempt int, direction, name, mediaType string, content []byte) error {
	sum := sha256.Sum256(content)
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO artifacts (run_id, attempt, direction, name, media_type, sha256, bytes, content)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		runID, attempt, direction, name, mediaType, hex.EncodeToString(sum[:]), len(content), content)
	return err
}

// ListArtifacts lists a run's artifacts.
func (s *Store) ListArtifacts(ctx context.Context, runID string) ([]Artifact, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text, run_id::text, attempt, direction, name, media_type, sha256, bytes, created_at
		FROM artifacts WHERE run_id = $1 ORDER BY attempt, direction, name`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Artifact])
}

// GetArtifactContent returns one artifact's bytes and media type.
func (s *Store) GetArtifactContent(ctx context.Context, id string) ([]byte, string, error) {
	var b []byte
	var mt string
	err := s.Pool.QueryRow(ctx, `SELECT content, media_type FROM artifacts WHERE id = $1`, id).Scan(&b, &mt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	return b, mt, err
}

// ---------------------------------------------------------------- memory

// Memory returns the agent's current memory, "" if it has none.
func (s *Store) Memory(ctx context.Context, agent string) (string, error) {
	var c string
	err := s.Pool.QueryRow(ctx, `SELECT content FROM memory_versions WHERE agent = $1 ORDER BY id DESC LIMIT 1`, agent).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return c, err
}

// MemoryVersion is one stored memory document.
type MemoryVersion struct {
	ID        int64     `json:"id"`
	RunID     *string   `json:"run_id,omitempty"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// MemoryHistory returns an agent's memory versions, newest first.
func (s *Store) MemoryHistory(ctx context.Context, agent string, limit int) ([]MemoryVersion, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.Pool.Query(ctx, `SELECT id, run_id::text, content, created_at FROM memory_versions
		WHERE agent = $1 ORDER BY id DESC LIMIT $2`, agent, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[MemoryVersion])
}

// ---------------------------------------------------------------- messages

// Message is one inbox entry.
type Message struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`
	Direction string    `json:"direction"`
	Body      string    `json:"body"`
	ReplyTo   *string   `json:"reply_to,omitempty"`
	RunID     *string   `json:"run_id,omitempty"`
	Author    *string   `json:"author,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// SendToAgent stores a person's message for the agent's next run.
func (s *Store) SendToAgent(ctx context.Context, agent, body, author string, replyTo *string) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO messages (agent, direction, body, author, reply_to)
		VALUES ($1, 'to_agent', $2, NULLIF($3, ''), $4) RETURNING id::text`,
		agent, body, author, replyTo).Scan(&id)
	if isForeignKeyViolation(err) {
		return "", ErrNotFound
	}
	return id, err
}

// ListMessages returns an agent's inbox, oldest first.
func (s *Store) ListMessages(ctx context.Context, agent string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `SELECT id::text, agent, direction, body, reply_to::text, run_id::text, author, created_at
		FROM (SELECT * FROM messages WHERE agent = $1 ORDER BY created_at DESC LIMIT $2) m
		ORDER BY created_at`, agent, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Message])
}

// openQuestion is true of a question (alias m) nobody has answered: no reply
// names it and no message reached the agent after it. Any later message
// closes it, because the run that reads that message sees the person's
// words and asks again if it still needs to.
const openQuestion = `m.direction = 'from_agent'
	AND NOT EXISTS (SELECT 1 FROM messages r
	                WHERE r.agent = m.agent AND r.direction = 'to_agent'
	                  AND (r.reply_to = m.id OR r.created_at > m.created_at))`

// ---------------------------------------------------------------- run context

// RunContext is what a starting run reads: the agent's memory and the
// messages waiting for it. Reading them marks the messages consumed by this
// run in the same transaction, so a message reaches exactly one run.
type RunContext struct {
	Memory   string
	Messages []Message
	// Questions are the agent's own unanswered questions, so it does not ask
	// them again.
	Questions []Message
}

// StartRunContext reads and consumes the run's context.
func (s *Store) StartRunContext(ctx context.Context, runID, agent string) (*RunContext, error) {
	rc := &RunContext{}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT content FROM memory_versions WHERE agent = $1 ORDER BY id DESC LIMIT 1`, agent).Scan(&rc.Memory)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		rows, err := tx.Query(ctx, `
			UPDATE messages SET run_id = $2
			WHERE agent = $1 AND direction = 'to_agent' AND run_id IS NULL
			RETURNING id::text, agent, direction, body, reply_to::text, run_id::text, author, created_at`, agent, runID)
		if err != nil {
			return err
		}
		if rc.Messages, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Message]); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT id::text, agent, direction, body, reply_to::text, run_id::text, author, created_at
			FROM messages m
			WHERE agent = $1 AND `+openQuestion+`
			ORDER BY created_at`, agent)
		if err != nil {
			return err
		}
		rc.Questions, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Message])
		return err
	})
	return rc, err
}

// ---------------------------------------------------------------- outcome

// ProposedAction is an action as the agent proposed it, with the rule the
// agent's spec gives its type.
type ProposedAction struct {
	Type   string
	Params json.RawMessage
	Reason string
	Rule   string
	// Error says why the harness denied it regardless of rule (unknown
	// type, invalid params).
	Error string
}

// Outcome is everything a successful run writes besides its row.
type Outcome struct {
	Memory    *string
	Questions []string
	Actions   []ProposedAction
}

// RecordOutcome stores memory, questions and actions together with the run's
// final status, so a run is never succeeded with half its outcome missing.
// It returns the stored action ids in input order.
func (s *Store) RecordOutcome(ctx context.Context, runID, agent string, res RunResult, o Outcome) ([]string, error) {
	var ids []string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := finishRun(ctx, tx, runID, res); err != nil {
			return err
		}
		if o.Memory != nil {
			if _, err := tx.Exec(ctx, `INSERT INTO memory_versions (agent, run_id, content) VALUES ($1, $2, $3)`,
				agent, runID, *o.Memory); err != nil {
				return err
			}
		}
		for _, q := range o.Questions {
			if _, err := tx.Exec(ctx, `INSERT INTO messages (agent, direction, body, run_id) VALUES ($1, 'from_agent', $2, $3)`,
				agent, q, runID); err != nil {
				return err
			}
		}
		for _, a := range o.Actions {
			status := map[string]string{"auto": "approved", "approve": "pending", "deny": "denied"}[a.Rule]
			var id string
			if err := tx.QueryRow(ctx, `
				INSERT INTO actions (run_id, agent, type, params, reason, rule, status, decided_by, decided_at, error)
				VALUES ($1, $2, $3, $4, $5, $6, $7,
				        CASE WHEN $6 = 'approve' THEN NULL ELSE 'rule' END,
				        CASE WHEN $6 = 'approve' THEN NULL ELSE now() END,
				        NULLIF($8, ''))
				RETURNING id::text`,
				runID, agent, a.Type, []byte(a.Params), a.Reason, a.Rule, status, a.Error).Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	return ids, err
}

// ---------------------------------------------------------------- actions

// Action is a stored proposal.
type Action struct {
	ID         string          `json:"id"`
	RunID      string          `json:"run_id"`
	Agent      string          `json:"agent"`
	Type       string          `json:"type"`
	Params     json.RawMessage `json:"params"`
	Reason     string          `json:"reason"`
	Rule       string          `json:"rule"`
	Status     string          `json:"status"`
	DecidedBy  *string         `json:"decided_by,omitempty"`
	DecidedAt  *time.Time      `json:"decided_at,omitempty"`
	ExecutedAt *time.Time      `json:"executed_at,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	Error      *string         `json:"error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

const actionCols = `id::text, run_id::text, agent, type, params, reason, rule, status, decided_by,
	decided_at, executed_at, result, error, created_at`

// GetAction returns one action.
func (s *Store) GetAction(ctx context.Context, id string) (*Action, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+actionCols+` FROM actions WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Action])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}

// ListActions returns actions newest first, optionally by agent and status.
func (s *Store) ListActions(ctx context.Context, agent, status string, limit int) ([]Action, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+actionCols+` FROM actions
		WHERE ($1 = '' OR agent = $1) AND ($2 = '' OR status = $2)
		ORDER BY created_at DESC LIMIT $3`, agent, status, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Action])
}

// DecideAction approves or rejects a pending action.
func (s *Store) DecideAction(ctx context.Context, id string, approve bool, by string) error {
	status := "rejected"
	if approve {
		status = "approved"
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE actions SET status = $2, decided_by = $3, decided_at = now()
		WHERE id = $1 AND status = 'pending'`, id, status, by)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.GetAction(ctx, id); err != nil {
			return err
		}
		return ErrConflict
	}
	return nil
}

// ClaimApprovedAction moves the oldest approved action to executing. An
// action is executed at most once: a crash mid-execution leaves it
// executing, never re-run (see FailOrphanedActions).
func (s *Store) ClaimApprovedAction(ctx context.Context) (*Action, error) {
	rows, err := s.Pool.Query(ctx, `
		UPDATE actions SET status = 'executing'
		WHERE id = (SELECT id FROM actions WHERE status = 'approved' ORDER BY decided_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+actionCols)
	if err != nil {
		return nil, err
	}
	a, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Action])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

// FinishAction records an execution's result.
func (s *Store) FinishAction(ctx context.Context, id string, result json.RawMessage, execErr error) error {
	status, msg := "executed", ""
	if execErr != nil {
		status, msg = "failed", execErr.Error()
	}
	_, err := s.Pool.Exec(ctx, `UPDATE actions SET status = $2, executed_at = now(), result = $3, error = NULLIF($4, '')
		WHERE id = $1 AND status = 'executing'`, id, status, nullJSON(result), msg)
	return err
}

// FailOrphanedActions fails actions left executing by a process that died.
// Whether the side effect happened is unknown, so they are failed for a
// person to check rather than retried: a retried webhook may act twice.
func (s *Store) FailOrphanedActions(ctx context.Context) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE actions SET status = 'failed', executed_at = now(),
		error = 'interrupted during execution; the side effect may or may not have happened'
		WHERE status = 'executing'`)
	return tag.RowsAffected(), err
}

// ---------------------------------------------------------------- deliveries

// RecordDelivery stores one notification attempt.
func (s *Store) RecordDelivery(ctx context.Context, runID, actionID *string, channel, purpose string, sendErr error) error {
	status, msg := "sent", ""
	if sendErr != nil {
		status, msg = "failed", sendErr.Error()
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO deliveries (run_id, action_id, channel, purpose, status, error)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`, runID, actionID, channel, purpose, status, msg)
	return err
}

// Delivery is a stored notification attempt.
type Delivery struct {
	ID          int64     `json:"id"`
	RunID       *string   `json:"run_id,omitempty"`
	ActionID    *string   `json:"action_id,omitempty"`
	Channel     string    `json:"channel"`
	Purpose     string    `json:"purpose"`
	Status      string    `json:"status"`
	Error       *string   `json:"error,omitempty"`
	AttemptedAt time.Time `json:"attempted_at"`
}

// ListDeliveries returns a run's notification attempts.
func (s *Store) ListDeliveries(ctx context.Context, runID string) ([]Delivery, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, run_id::text, action_id::text, channel, purpose, status, error, attempted_at
		FROM deliveries WHERE run_id = $1 ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Delivery])
}

// ---------------------------------------------------------------- helpers

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23503"
}

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
