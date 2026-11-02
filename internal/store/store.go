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

	"github.com/kensac/orbit/internal/status"
	"github.com/kensac/orbit/internal/tokens"
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

// MigrateTo applies the embedded migrations up to version, for tests of an
// upgrade over existing rows.
func (s *Store) MigrateTo(ctx context.Context, version int64) error {
	db := stdlib.OpenDBFromPool(s.Pool)
	defer db.Close()
	p, err := newProvider(db)
	if err != nil {
		return err
	}
	_, err = p.UpTo(ctx, version)
	return err
}

// newProvider is the one goose provider. Out-of-order is allowed because agents land migrations in parallel: new files are named by UTC timestamp (YYYYMMDDHHMMSS_name.sql, docs/agents/store.md), so a migration merged after a later-numbered one still applies instead of failing the start.
func newProvider(db *sql.DB) (*goose.Provider, error) {
	return goose.NewProvider(goose.DialectPostgres, db, mustSub(migrations, "migrations"), goose.WithAllowOutofOrder(true))
}

func migrate(ctx context.Context, db *sql.DB) error {
	p, err := newProvider(db)
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
			if _, err := tx.Exec(ctx, `INSERT INTO conversations (agent, title, is_default) VALUES ($1, 'General', true)
				ON CONFLICT (agent) WHERE is_default DO NOTHING`, name); err != nil {
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
	// NeedsInput is true while the agent waits on its person: an open
	// question or an action pending approval.
	NeedsInput bool `json:"needs_input"`
	// AgentStatus is the agent's status (status.Aggregate over its current
	// run: the running one, else the newest).
	AgentStatus status.Status `json:"agent_status"`
	// Origin and Version are where the configuration in force defines the
	// agent (file, database, builtin) and a database agent's version; the
	// API fills them, empty for an agent no longer configured.
	Origin  string `json:"origin,omitempty"`
	Version int    `json:"version,omitempty"`
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
		         WHERE m.agent = a.name AND `+openQuestion+`
			           AND NOT EXISTS (SELECT 1 FROM tasks k WHERE k.conversation_id = m.conversation_id)),
		       (SELECT count(*) FROM actions x WHERE x.agent = a.name AND x.status = 'pending'),
		       `+currentRun("a.name", "sr.agent = a.name")+`
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
		var id, runStatus, trigger *string
		var created *time.Time
		var finished *time.Time
		var summary *string
		var current *RunFacts
		if err := rows.Scan(&a.Name, &a.SpecSHA, &a.Spec, &a.Present, &a.Enabled, &a.FirstSeen, &a.UpdatedAt,
			&id, &runStatus, &trigger, &created, &finished, &summary, &a.Unread, &a.Pending, &current); err != nil {
			return nil, err
		}
		a.NeedsInput = a.Unread > 0 || a.Pending > 0
		a.AgentStatus = StatusFacts{Run: current, OpenQuestion: a.Unread > 0, PendingAction: a.Pending > 0}.Status()
		if id != nil {
			a.LastRun = &RunSummary{ID: *id, Status: *runStatus, Trigger: *trigger, CreatedAt: *created, FinishedAt: finished, Summary: summary}
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
	// ConversationID is the conversation the run answers in; empty is the
	// agent's default conversation (schedules, manual runs).
	ConversationID string
	// RunAfter holds the run until then (a scheduled run deferred at
	// enqueue, DeferRun); nil is claimable at once.
	RunAfter *time.Time
}

// EnqueueRun inserts a queued run. For a schedule fire already recorded it
// returns ("", nil): the fire was handled before, by this process or another.
func (s *Store) EnqueueRun(ctx context.Context, r NewRun) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO runs (agent, spec_sha, trigger, trigger_index, scheduled_for, prompt, conversation_id, run_after)
		VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, '')::uuid, $8)
		ON CONFLICT (agent, trigger_index, scheduled_for) WHERE trigger = 'schedule' DO NOTHING
		RETURNING id::text`,
		r.Agent, r.SpecSHA, r.Trigger, r.TriggerIndex, r.ScheduledFor, r.Prompt, r.ConversationID, r.RunAfter).Scan(&id)
	if isForeignKeyViolation(err) {
		return "", ErrNotFound
	}
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
	// ConversationID is the conversation the run belongs to: its inbox,
	// its open questions, the thread it is shown and the session it
	// continues are that conversation's.
	ConversationID string
	// TriggerIndex and ScheduledFor are which trigger fired and when, on a schedule run (a webhook run has the index only).
	TriggerIndex *int
	ScheduledFor *time.Time
}

// MaxAttempts bounds how often an interrupted run is claimed again. A run
// that kills its own worker (out of memory) would otherwise loop forever.
const MaxAttempts = 3

// ClaimRun moves the oldest queued run whose lane has nothing running (laneOf) to
// running, owned by worker, and records the claim as a new attempt. It
// returns nil when there is nothing to claim. FOR NO KEY UPDATE SKIP
// LOCKED gives one row to one claimer across processes, and does not skip
// a run another transaction only references (an event or journal row of a
// superseded attempt being written holds a KEY SHARE lock on it, which FOR
// UPDATE would skip); runs_one_running_lane is what makes
// "nothing running" true under concurrency: a second claimer for the same
// agent (or a chat agent's same conversation) fails the index and tries the
// next row.
func (s *Store) ClaimRun(ctx context.Context, worker string) (*ClaimedRun, error) {
	return s.ClaimRunFor(ctx, Claim{Worker: worker, Any: true, Builds: true})
}

// laneOf is the lane a run of table alias t claims (runs_one_running_lane):
// its conversation for a chat agent, whose conversations run side by side,
// else ” (one run per agent). Chat is read from the agent's recorded spec.
func laneOf(t string) string {
	return `COALESCE((SELECT CASE WHEN (sp.spec->>'chat')::boolean THEN ` + t + `.conversation_id::text ELSE '' END
	    FROM agents a JOIN agent_specs sp ON sp.sha = a.spec_sha WHERE a.name = ` + t + `.agent), '')`
}

// claim is ClaimRun with filter (SQL over the candidate q, using $2 to $4)
// narrowing the candidates (ClaimRunFor).
func (s *Store) claim(ctx context.Context, worker, filter string, args ...any) (*ClaimedRun, error) {
	for try := 0; try < 5; try++ {
		var c ClaimedRun
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `
				UPDATE runs SET status = 'running', worker = $1, started_at = now(), heartbeat_at = now(),
				       attempt = attempt + 1, lane = `+laneOf("runs")+`
				WHERE id = (
				    SELECT q.id FROM runs q
				    WHERE q.status = 'queued'
				      AND (q.run_after IS NULL OR q.run_after <= now())
				      AND NOT EXISTS (SELECT 1 FROM runs b WHERE b.agent = q.agent AND b.status = 'running'
				                      AND COALESCE(b.lane, '') = `+laneOf("q")+`)
				      `+filter+`
				    ORDER BY q.created_at
				    LIMIT 1
				    FOR NO KEY UPDATE SKIP LOCKED
				)
				RETURNING id::text, attempt, agent, spec_sha, trigger, prompt, chain, conversation_id::text, trigger_index, scheduled_for`, append([]any{worker}, args...)...).
				Scan(&c.ID, &c.Attempt, &c.Agent, &c.SpecSHA, &c.Trigger, &c.Prompt, &c.Chain, &c.ConversationID, &c.TriggerIndex, &c.ScheduledFor); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO run_attempts (run_id, attempt, worker) VALUES ($1, $2, $3)`,
				c.ID, c.Attempt, worker)
			return err
		})
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

// Heartbeat marks a running attempt alive. ErrConflict means the attempt no
// longer holds the run (the reaper requeued it, and another worker may have
// claimed it): its executor must stop, and everything it would still write
// is refused anyway.
func (s *Store) Heartbeat(ctx context.Context, runID string, attempt int) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE runs SET heartbeat_at = now()
		WHERE id = $1 AND status = 'running' AND attempt = $2`, runID, attempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("heartbeat of run %s attempt %d: %w (no longer its claim)", runID, attempt, ErrConflict)
	}
	return nil
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
			if err := endAttempt(ctx, tx, r.ID, r.Attempt, "lost"); err != nil {
				return err
			}
			if r.Attempt < MaxAttempts {
				if err := requeue(ctx, tx, r.ID, r.Attempt); err != nil {
					return err
				}
				continue
			}
			if err := finishRun(ctx, tx, r.ID, RunResult{Status: "failed", Attempt: r.Attempt,
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

// HandBack returns a running attempt's run to the queue: its executor
// stopped it (a drain past its cap, or a lost claim). Nothing a run does
// outside its own rows happens before it finishes (actions and
// notifications follow RecordOutcome), so another attempt is safe. cost and
// sessionCost are what the backend had spent when it stopped, when known.
// ErrConflict means the attempt no longer held the run.
func (s *Store) HandBack(ctx context.Context, runID string, attempt int, cost, sessionCost *float64) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := requeue(ctx, tx, runID, attempt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE run_attempts SET outcome = 'handed_back', ended_at = now(),
			cost_usd = $3, session_cost_usd = $4
			WHERE run_id = $1 AND attempt = $2`, runID, attempt, cost, sessionCost); err != nil {
			return err
		}
		return syncInTx(ctx, tx, runID, attempt)
	})
}

func requeue(ctx context.Context, tx pgx.Tx, runID string, attempt int) error {
	tag, err := tx.Exec(ctx, `UPDATE runs SET status = 'queued', worker = NULL, started_at = NULL, heartbeat_at = NULL
		WHERE id = $1 AND status = 'running' AND attempt = $2`, runID, attempt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("requeue run %s attempt %d: %w (no longer its claim)", runID, attempt, ErrConflict)
	}
	return releaseMessages(ctx, tx, runID)
}

// endAttempt records how an attempt ended. An attempt claimed before
// run_attempts existed has no row, which is not an error.
func endAttempt(ctx context.Context, tx pgx.Tx, runID string, attempt int, outcome string) error {
	if _, err := tx.Exec(ctx, `UPDATE run_attempts SET outcome = $3, ended_at = now()
		WHERE run_id = $1 AND attempt = $2 AND outcome IS NULL`, runID, attempt, outcome); err != nil {
		return err
	}
	return syncInTx(ctx, tx, runID, attempt)
}

// releaseMessages hands a run's consumed messages back to the inbox, so the
// next run reads what this one did not finish with. The attempt's read rows
// and messages.run_id are kept: they are the record that it read them.
func releaseMessages(ctx context.Context, db execer, runID string) error {
	if _, err := db.Exec(ctx, `UPDATE message_reads SET released_at = now() WHERE run_id = $1 AND released_at IS NULL`, runID); err != nil {
		return err
	}
	// A read without a row (giveComments, an older image) is recorded as
	// handed back here, or the message would stay held (waitingMessage).
	_, err := db.Exec(ctx, `INSERT INTO message_reads (message_id, run_id, attempt, read_at, released_at)
		SELECT m.id, r.id, r.attempt, COALESCE(r.started_at, m.created_at), now()
		FROM messages m JOIN runs r ON r.id = m.run_id
		WHERE m.run_id = $1 AND m.direction = 'to_agent'
		  AND NOT EXISTS (SELECT 1 FROM message_reads x WHERE x.message_id = m.id)`, runID)
	return err
}

// waitingMessage is true of a person's message (alias m) that no attempt
// holds, so the next run of its conversation reads it: never read, or
// handed back by every attempt that read it (message_reads). A message
// with run_id set and no read row was marked read without an attempt (a
// build given review comments when enqueued, giveComments) or by an image
// older than message_reads, which may still run during a deploy; it is held.
const waitingMessage = `m.direction = 'to_agent' AND (m.run_id IS NULL OR (
	NOT EXISTS (SELECT 1 FROM message_reads x WHERE x.message_id = m.id AND x.released_at IS NULL)
	AND EXISTS (SELECT 1 FROM message_reads x WHERE x.message_id = m.id)))`

// SetRunner records the backend and the spec an attempt executes under,
// on the run and on the attempt. The spec can differ from the one it was
// enqueued with when configuration reloaded while it waited; the row names
// the one it actually ran. ErrConflict means the attempt no longer holds
// the run.
func (s *Store) SetRunner(ctx context.Context, runID string, attempt int, kind, model, specSHA string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE runs SET runner_kind = $3, model = NULLIF($4, ''), spec_sha = $5
			WHERE id = $1 AND status = 'running' AND attempt = $2`, runID, attempt, kind, model, specSHA)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("set runner of run %s attempt %d: %w (no longer its claim)", runID, attempt, ErrConflict)
		}
		_, err = tx.Exec(ctx, `UPDATE run_attempts SET runner_kind = $3, spec_sha = $4 WHERE run_id = $1 AND attempt = $2`,
			runID, attempt, kind, specSHA)
		return err
	})
}

// hookSnapshot reads a stored hook_status; nil when there is none or it
// does not parse (status then falls back to the run's row).
func hookSnapshot(b []byte) *status.Snapshot {
	if len(b) == 0 {
		return nil
	}
	var s status.Snapshot
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return &s
}

// SetHookStatus records what the run's hooks last reported (a
// status.Snapshot, as JSON) while the attempt still holds the running run.
// ErrConflict means it no longer does: a report that arrives after the run
// finished, or from a superseded attempt, is not the run's status.
func (s *Store) SetHookStatus(ctx context.Context, runID string, attempt int, snapshot json.RawMessage) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE runs SET hook_status = $3
		WHERE id = $1 AND status = 'running' AND attempt = $2`, runID, attempt, snapshot)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("set hook status of run %s attempt %d: %w (no longer its claim)", runID, attempt, ErrConflict)
	}
	return nil
}

// RunResult is a finished run's outcome.
type RunResult struct {
	// Attempt is the attempt finishing the run; only the attempt holding
	// the claim can.
	Attempt   int
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
	// Tokens is this attempt's token counts, nil when the backend reported none.
	Tokens *tokens.Usage
	// Gate names what ended a skipped run without a backend call; set
	// exactly when Status is skipped (runs_skipped_names_gate).
	Gate string
}

// Run statuses that end a run.
const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	// StatusSkipped is a run a gate ended without calling a backend: it
	// finished well, and judged nothing.
	StatusSkipped = "skipped"
	// GateMissed names a schedule fire that was claimed past its grace and skipped rather than run late. Unlike a gate's skip it is no success: the agent did nothing.
	GateMissed = "missed"
)

// FinishRun records the outcome of a run that wrote nothing else (a failure,
// or a success with no memory, questions or actions).
func (s *Store) FinishRun(ctx context.Context, runID string, r RunResult) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := finishRun(ctx, tx, runID, r); err != nil {
			return err
		}
		if r.Status == StatusFailed {
			return releaseMessages(ctx, tx, runID)
		}
		return nil
	})
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// finishRun is the one write of a run's final state. Only the running
// attempt that holds the claim can finish it, so a run the reaper already
// failed stays failed, and an attempt that lost its claim to a newer one
// writes nothing. The run's cost is every attempt's: this one's (r.CostUSD)
// plus what earlier, interrupted attempts were recorded to have spent.
func finishRun(ctx context.Context, tx pgx.Tx, runID string, r RunResult) error {
	tag, err := tx.Exec(ctx, `
		UPDATE runs SET status = $2, finished_at = now(), error = NULLIF($3, ''),
		       summary = NULLIF($4, ''), report = NULLIF($5, ''), output = $6, usage = $7,
		       cost_usd = CASE WHEN $8::numeric IS NULL THEN NULL ELSE $8::numeric + COALESCE(
		           (SELECT sum(a.cost_usd) FROM run_attempts a WHERE a.run_id = $1 AND a.attempt < $11), 0) END,
		       session_id = NULLIF($9, ''), session_cost_usd = $10, gate = NULLIF($12, '')
		WHERE id = $1 AND status = 'running' AND attempt = $11`,
		runID, r.Status, r.Error, r.Summary, r.Report, nullJSON(r.Output), nullJSON(r.Usage), r.CostUSD, r.SessionID,
		r.SessionCostUSD, r.Attempt, r.Gate)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("finish run %s attempt %d: %w (not its running claim)", runID, r.Attempt, ErrConflict)
	}
	if _, err = tx.Exec(ctx, `UPDATE run_attempts SET outcome = $3, ended_at = now(), cost_usd = $4, session_cost_usd = $5,
		session_id = COALESCE(NULLIF($6, ''), session_id)
		WHERE run_id = $1 AND attempt = $2 AND outcome IS NULL`,
		runID, r.Attempt, r.Status, r.CostUSD, r.SessionCostUSD, r.SessionID); err != nil {
		return err
	}
	if err := wakeForSteers(ctx, tx, runID); err != nil {
		return err
	}
	if err := recordTokens(ctx, tx, runID, r.Attempt, r.Tokens); err != nil {
		return err
	}
	if r.Status == "failed" {
		// Every way a run fails ends here (the engine, a build, the reaper).
		if err := newAttention(ctx, tx, AttentionFailure, "failure:"+runID, runID, " run failed", nil, nil, nil, nil); err != nil {
			return err
		}
	}
	if r.Attempt < 1 {
		return nil
	}
	return syncInTx(ctx, tx, runID, r.Attempt)
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
	// Steps is the number of tool calls in the latest attempt's
	// timeline, as a thread's run item counts them (stepsCount).
	Steps int `json:"steps"`
	// Gate names what ended a skipped run.
	Gate *string `json:"gate,omitempty"`
	// HookStatus is what the run's hooks last reported (status.Snapshot).
	HookStatus json.RawMessage `json:"-"`
	// AgentStatus is the agent's status in this run (status.Derive).
	AgentStatus status.Status `json:"agent_status"`
	// Tokens is every attempt's token counts together; Prefix the stable prefix the latest attempt sent, against the conversation's previous run.
	Tokens *Tokens `json:"tokens,omitempty"`
	Prefix *Prefix `json:"prefix,omitempty"`
}

// runCols reads a run aliased r: stepsCount names it.
var runCols = `r.id::text, r.attempt, r.agent, r.spec_sha, r.trigger, r.trigger_index, r.scheduled_for, r.prompt, r.status,
	r.runner_kind, r.model, r.worker, r.created_at, r.started_at, r.finished_at, r.error, r.summary, r.report,
	r.output, r.usage, r.cost_usd::float8, r.session_id, r.session_mode, r.session_parent, r.session_depth,
	r.run_after, r.continues::text, r.chain, r.gate, r.hook_status,
	EXISTS (SELECT 1 FROM messages m WHERE m.run_id = r.id AND ` + personQuestion + `),
	EXISTS (SELECT 1 FROM actions pa WHERE pa.run_id = r.id AND pa.status = 'pending'), ` + stepsCount + `, ` + runTokens + `, ` + runPrefix

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	var prefix []byte
	var openQ, pending bool
	err := row.Scan(&r.ID, &r.Attempt, &r.Agent, &r.SpecSHA, &r.Trigger, &r.TriggerIndex, &r.ScheduledFor, &r.Prompt,
		&r.Status, &r.RunnerKind, &r.Model, &r.Worker, &r.CreatedAt, &r.StartedAt, &r.FinishedAt, &r.Error,
		&r.Summary, &r.Report, &r.Output, &r.Usage, &r.CostUSD, &r.SessionID, &r.SessionMode, &r.SessionParent,
		&r.SessionDepth, &r.RunAfter, &r.Continues, &r.Chain, &r.Gate, &r.HookStatus, &openQ, &pending, &r.Steps, &r.Tokens, &prefix)
	if err == nil {
		r.AgentStatus = status.Derive(status.Run{ID: r.ID, Status: r.Status, Attempt: r.Attempt, CreatedAt: r.CreatedAt,
			StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Error: r.Error, OpenQuestion: openQ, PendingAction: pending,
			Hook: hookSnapshot(r.HookStatus)})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if r.Prefix, err = decodePrefix(prefix); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetRun returns one run.
func (s *Store) GetRun(ctx context.Context, id string) (*Run, error) {
	return scanRun(s.Pool.QueryRow(ctx, `SELECT `+runCols+` FROM runs r WHERE r.id = $1`, id))
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
	f.Limit = clampLimit(f.Limit, 50, 500)
	rows, err := s.Pool.Query(ctx, `SELECT `+runCols+` FROM runs r
		WHERE ($1 = '' OR r.agent = $1) AND ($2 = '' OR r.status = $2)
		  AND ($3::timestamptz IS NULL OR r.created_at < $3)
		ORDER BY r.created_at DESC LIMIT $4`, f.Agent, f.Status, f.Before, f.Limit)
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

// CancelRun cancels a queued run at once (status cancelled), or asks a
// running one to stop (status "cancelling"): its executor owns it, and ends
// it. Any other state is ErrConflict.
func (s *Store) CancelRun(ctx context.Context, id, by string) (status string, err error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE runs SET status = 'cancelled', finished_at = now(),
		cancel_requested_at = now(), cancel_requested_by = NULLIF($2, '')
		WHERE id = $1 AND status = 'queued'`, id, by)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 1 {
		return StatusCancelled, nil
	}
	// A running run is asked to stop: its attempt sees the request
	// (CancelRequested), interrupts its backend and ends the run cancelled.
	tag, err = s.Pool.Exec(ctx, `UPDATE runs SET cancel_requested_at = COALESCE(cancel_requested_at, now()),
		cancel_requested_by = COALESCE(cancel_requested_by, NULLIF($2, ''))
		WHERE id = $1 AND status = 'running'`, id, by)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", ErrConflict
	}
	return "cancelling", nil
}

// CancelRequested reports whether a person asked the run to stop.
func (s *Store) CancelRequested(ctx context.Context, id string) (bool, error) {
	var asked bool
	err := s.Pool.QueryRow(ctx, `SELECT cancel_requested_at IS NOT NULL FROM runs WHERE id = $1`, id).Scan(&asked)
	return asked, err
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

// AppendEvent stores the next transcript entry and projects it into the
// run's journal. Payloads that are not valid JSON are stored as a JSON
// string, so a malformed line from a runner is kept rather than dropped.
// An error wrapping ErrJournal means the event was stored and only its
// journal rows wait for a sync.
func (s *Store) AppendEvent(ctx context.Context, runID string, attempt, seq int, kind string, payload []byte) error {
	if !json.Valid(payload) {
		payload, _ = json.Marshal(string(payload))
	}
	var projectErr error
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO run_events (run_id, attempt, seq, kind, payload) VALUES ($1, $2, $3, $4, $5)`,
			runID, attempt, seq, kind, payload); err != nil {
			return err
		}
		// The journal row commits with its event. A projection that fails
		// keeps the event (the transcript is the record) and is retried
		// by the next sync.
		projectErr = pgx.BeginFunc(ctx, tx, func(sp pgx.Tx) error { return syncInTx(ctx, sp, runID, attempt) })
		return nil
	})
	if err == nil && projectErr != nil {
		err = fmt.Errorf("%w: run %s attempt %d event %d: %w", ErrJournal, runID, attempt, seq, projectErr)
	}
	return err
}

// ListEvents returns one attempt's transcript from seq after. Attempt 0 means
// the run's latest attempt.
func (s *Store) ListEvents(ctx context.Context, runID string, attempt, after, limit int) ([]Event, error) {
	limit = clampLimit(limit, 1000, 5000)
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

// OpenArtifact returns one artifact and a reader of its content.
func (s *Store) OpenArtifact(ctx context.Context, id string) (Artifact, *Blob, error) {
	var a Artifact
	err := s.Pool.QueryRow(ctx, `SELECT id::text, run_id::text, attempt, direction, name, media_type, sha256, bytes, created_at
		FROM artifacts WHERE id = $1`, id).
		Scan(&a.ID, &a.RunID, &a.Attempt, &a.Direction, &a.Name, &a.MediaType, &a.SHA256, &a.Bytes, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, nil, ErrNotFound
	}
	if err != nil {
		return a, nil, err
	}
	return a, s.blob(ctx, blobArtifact, a.ID, a.Bytes), nil
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
	// Author is the person who wrote or restored it through the API; RestoredFrom the version a restore copied.
	Author       *string `json:"author"`
	RestoredFrom *int64  `json:"restored_from"`
}

// MemoryHistory returns an agent's memory versions, newest first.
func (s *Store) MemoryHistory(ctx context.Context, agent string, limit int) ([]MemoryVersion, error) {
	limit = clampLimit(limit, 20, 200)
	rows, err := s.Pool.Query(ctx, `SELECT id, run_id::text, content, created_at, author, restored_from FROM memory_versions
		WHERE agent = $1 ORDER BY id DESC LIMIT $2`, agent, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[MemoryVersion])
}

// WriteMemory writes the next version of an agent's memory document: content as given, or, with restore above 0, a copy of that earlier version of the same agent's (ErrNotFound for another's). Nothing is replaced; the new version is the newest.
func (s *Store) WriteMemory(ctx context.Context, agent, content string, restore int64, author string) (*MemoryVersion, error) {
	var v MemoryVersion
	var err error
	if restore > 0 {
		err = s.Pool.QueryRow(ctx, `INSERT INTO memory_versions (agent, content, author, restored_from)
			SELECT agent, content, NULLIF($3, ''), id FROM memory_versions WHERE id = $2 AND agent = $1
			RETURNING id, run_id::text, content, created_at, author, restored_from`, agent, restore, author).
			Scan(&v.ID, &v.RunID, &v.Content, &v.CreatedAt, &v.Author, &v.RestoredFrom)
	} else {
		err = s.Pool.QueryRow(ctx, `INSERT INTO memory_versions (agent, content, author) VALUES ($1, $2, NULLIF($3, ''))
			RETURNING id, run_id::text, content, created_at, author, restored_from`, agent, content, author).
			Scan(&v.ID, &v.RunID, &v.Content, &v.CreatedAt, &v.Author, &v.RestoredFrom)
	}
	if errors.Is(err, pgx.ErrNoRows) || isForeignKeyViolation(err) {
		return nil, ErrNotFound
	}
	return &v, err
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
	r, _, err := s.PostMessageOnce(ctx, PostMessage{Agent: agent, Body: body, Author: author, ReplyTo: replyTo})
	return r.MessageID, err
}

// ListMessages returns an agent's inbox, oldest first.
func (s *Store) ListMessages(ctx context.Context, agent string, limit int) ([]Message, error) {
	limit = clampLimit(limit, 100, 500)
	rows, err := s.Pool.Query(ctx, `SELECT id::text, agent, direction, body, reply_to::text, run_id::text, author, created_at
		FROM (SELECT * FROM messages
		      WHERE conversation_id = (SELECT id FROM conversations WHERE agent = $1 AND is_default)
		      ORDER BY created_at DESC LIMIT $2) m
		ORDER BY created_at`, agent, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Message])
}

// openQuestion is true of a question (alias m) nobody has answered: no reply
// names it and no message reached its conversation after it. Any later
// message there closes it, because the run that reads that message sees the
// person's words and asks again if it still needs to.
const openQuestion = `m.direction = 'from_agent'
	AND NOT EXISTS (SELECT 1 FROM messages r
	                WHERE r.conversation_id = m.conversation_id AND r.direction = 'to_agent'
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

// StartRunContext reads and consumes the run's context: the agent's memory,
// and the unread messages and open questions of the run's conversation
// only, so a run never reads another conversation's words.
func (s *Store) StartRunContext(ctx context.Context, runID, agent string) (*RunContext, error) {
	rc := &RunContext{}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT content FROM memory_versions WHERE agent = $1 ORDER BY id DESC LIMIT 1`, agent).Scan(&rc.Memory)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// One running run per agent (runs_one_running) is the only reader
		// of its conversations' messages; message_reads_one_holder holds it.
		rows, err := tx.Query(ctx, `
			UPDATE messages m SET run_id = $2
			WHERE m.agent = $1 AND `+waitingMessage+`
			  AND m.conversation_id = (SELECT conversation_id FROM runs WHERE id = $2)
			RETURNING id::text, agent, direction, body, reply_to::text, run_id::text, author, created_at`, agent, runID)
		if err != nil {
			return err
		}
		if rc.Messages, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Message]); err != nil {
			return err
		}
		ids := make([]string, len(rc.Messages))
		for i, m := range rc.Messages {
			ids[i] = m.ID
		}
		// A read an older image handed back (run_id = NULL) left its row
		// open; it ends where this one starts.
		if _, err := tx.Exec(ctx, `UPDATE message_reads SET released_at = now()
			WHERE message_id = ANY($1::uuid[]) AND released_at IS NULL`, ids); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO message_reads (message_id, run_id, attempt)
			SELECT unnest($1::uuid[]), id, attempt FROM runs WHERE id = $2`, ids, runID); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT id::text, agent, direction, body, reply_to::text, run_id::text, author, created_at
			FROM messages m
			WHERE agent = $1 AND conversation_id = (SELECT conversation_id FROM runs WHERE id = $2) AND `+openQuestion+`
			ORDER BY created_at`, agent, runID)
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
	// InstructedBy is the person whose message the run answered. An
	// approve-rule action they asked for is approved by them.
	InstructedBy string
}

// Outcome is everything a successful run writes besides its row.
type Outcome struct {
	Memory    *string
	Questions []string
	Actions   []ProposedAction
	// Plan is a plan the run proposed for its ticket (tickets.go).
	Plan *PlanProposal
	// Entries are the memory entry writes the run made, applied in order; each one's Result is set (entry_writes.go).
	Entries []*EntryWrite
	// Skills are the skill writes the run made (skills.go), applied after its entries.
	Skills []*AgentSkillWrite
}

// PlanProposal is a ticket plan a run proposed. RecordOutcome sets Version
// to the stored version, or leaves it 0 when the ticket could no longer take
// a plan (it moved on while the run worked), which keeps the run's outcome.
type PlanProposal struct {
	TicketID string
	Body     string
	Version  int
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
		// Questions and actions take clock_timestamp(), not the
		// transaction's now(), so each has its own time and the thread
		// orders them as the run listed them.
		for _, q := range o.Questions {
			// A question is asked in the asking run's conversation.
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO messages (agent, direction, body, run_id, conversation_id, created_at)
				SELECT $1, 'from_agent', $2, $3, conversation_id, clock_timestamp() FROM runs WHERE id = $3
				RETURNING id::text`, agent, q, runID).Scan(&id); err != nil {
				return err
			}
			if err := newAttention(ctx, tx, AttentionQuestion, "question:"+id, runID, " has a question", nil, &id, nil, nil); err != nil {
				return err
			}
		}
		if p := o.Plan; p != nil {
			v, err := proposePlan(ctx, tx, p.TicketID, runID, p.Body)
			if err != nil && !errors.Is(err, ErrConflict) {
				return err
			}
			p.Version = v
		}
		if err := applyEntryWrites(ctx, tx, runID, o.Entries); err != nil {
			return err
		}
		if err := applySkillWrites(ctx, tx, runID, agent, o.Skills); err != nil {
			return err
		}
		for _, a := range o.Actions {
			id, err := insertAction(ctx, tx, runID, agent, a)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	return ids, err
}

// insertAction stores one proposal with the state its rule gives it: auto
// arrives approved by rule, approve waits for a person, deny is recorded
// and never executed.
func insertAction(ctx context.Context, tx pgx.Tx, runID, agent string, a ProposedAction) (string, error) {
	status := map[string]string{"auto": "approved", "approve": "pending", "deny": "denied"}[a.Rule]
	decidedBy := "rule"
	switch {
	case a.Rule == "approve" && a.InstructedBy != "" && a.Error == "":
		status, decidedBy = "approved", a.InstructedBy
	case a.Rule == "approve":
		decidedBy = ""
	}
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO actions (run_id, agent, type, params, reason, rule, status, decided_by, decided_at, error, created_at, instructed_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''),
		        CASE WHEN $8 = '' THEN NULL ELSE now() END,
		        NULLIF($9, ''), clock_timestamp(), NULLIF($10, ''))
		RETURNING id::text`,
		runID, agent, a.Type, []byte(a.Params), a.Reason, a.Rule, status, decidedBy, a.Error, a.InstructedBy).Scan(&id)
	if err != nil || status != "pending" {
		return id, err
	}
	return id, newAttention(ctx, tx, AttentionApproval, "action:"+id, runID, " wants to "+a.Type, &id, nil, nil, nil)
}

// StartActionNow records an action a running chat turn executes at once
// (its rule auto, or approve and its person asked for it): the row starts
// executing, decided by the rule or the person, and FinishAction ends it.
// Only the run's own running claim may do this.
func (s *Store) StartActionNow(ctx context.Context, runID string, attempt int, agent string, a ProposedAction) (string, error) {
	var running bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE id = $1 AND status = 'running' AND attempt = $2)`,
		runID, attempt).Scan(&running); err != nil {
		return "", err
	}
	if !running {
		return "", fmt.Errorf("run %s attempt %d: %w (not its running claim)", runID, attempt, ErrConflict)
	}
	var id string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		if id, err = insertAction(ctx, tx, runID, agent, a); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE actions SET status = 'executing', executing_since = now() WHERE id = $1 AND status = 'approved'`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("action %s was not approved: %w", id, ErrConflict)
		}
		return nil
	})
	return id, err
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
	// InstructedBy is the person whose message the action answers: they
	// asked for it in the conversation, which approves an action whose
	// rule is approve.
	InstructedBy *string `json:"instructed_by,omitempty"`
}

const actionCols = `id::text, run_id::text, agent, type, params, reason, rule, status, decided_by,
	decided_at, executed_at, result, error, created_at, instructed_by`

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
	page, err := s.ListActionsPage(ctx, ActionFilter{Agent: agent, Status: status, Limit: limit})
	if err != nil {
		return nil, err
	}
	return page.Actions, nil
}

// ActionFilter narrows ListActionsPage. Empty fields do not filter; Before
// is a cursor from a previous page's NextBefore.
type ActionFilter struct {
	Agent  string
	Status string
	RunID  string
	Before *ConversationCursor
	Limit  int
}

// ActionPage is a page of actions, newest first. HasMore says older ones
// exist; NextBefore is the cursor that fetches them.
type ActionPage struct {
	Actions    []Action
	HasMore    bool
	NextBefore string
}

// DefaultActionLimit and MaxActionLimit are an action page's size.
const (
	DefaultActionLimit = 100
	MaxActionLimit     = 500
)

// ListActionsPage returns actions newest first, ordered by (created_at,
// id), so paging by the cursor never skips or repeats one.
func (s *Store) ListActionsPage(ctx context.Context, f ActionFilter) (*ActionPage, error) {
	f.Limit = clampLimit(f.Limit, DefaultActionLimit, MaxActionLimit)
	var at, runID any
	id := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if f.Before != nil {
		at, id = f.Before.At, f.Before.ID
	}
	if f.RunID != "" {
		runID = f.RunID
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+actionCols+` FROM actions
		WHERE ($1 = '' OR agent = $1) AND ($2 = '' OR status = $2) AND ($3::uuid IS NULL OR run_id = $3::uuid)
		  AND (created_at, id) < (COALESCE($4::timestamptz, 'infinity'), $5::uuid)
		ORDER BY created_at DESC, id DESC LIMIT $6`, f.Agent, f.Status, runID, at, id, f.Limit+1)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Action])
	if err != nil {
		return nil, err
	}
	page := &ActionPage{Actions: list}
	if len(list) > f.Limit {
		page.Actions, page.HasMore = list[:f.Limit], true
		last := list[f.Limit-1]
		page.NextBefore = ConversationCursor{At: last.CreatedAt, ID: last.ID}.String()
	}
	return page, nil
}

// DecideAction approves or rejects a pending action.
func (s *Store) DecideAction(ctx context.Context, id string, approve bool, by string) error {
	_, err := s.DecideActionOnce(ctx, id, approve, by, "")
	return err
}

// ClaimApprovedAction moves the oldest approved action to executing. An
// action is executed at most once: a crash mid-execution leaves it
// executing, never re-run (see FailOrphanedActions).
func (s *Store) ClaimApprovedAction(ctx context.Context) (*Action, error) {
	rows, err := s.Pool.Query(ctx, `
		UPDATE actions SET status = 'executing', executing_since = now()
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

// FailOrphanedActions fails actions left executing by a process that died:
// those executing for longer than olderThan, which the caller sets above
// the longest an execution may take, so an action another live process is
// executing (two run at once during a deploy) is never touched. One claimed
// before executing_since existed is timed from its decision. Whether the
// side effect happened is unknown, so they are failed for a person to check
// rather than retried: a retried webhook may act twice.
func (s *Store) FailOrphanedActions(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE actions SET status = 'failed', executed_at = now(),
		error = 'interrupted during execution; the side effect may or may not have happened'
		WHERE status = 'executing' AND COALESCE(executing_since, decided_at) < now() - $1::interval`, olderThan.String())
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

// clampLimit is a list's page size: def when unset, at most max. A larger
// request is clamped, not reset, so a client asking for more gets the most.
func clampLimit(limit, def, most int) int {
	if limit <= 0 {
		return def
	}
	return min(limit, most)
}

func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}

// IsBadID reports whether err is Postgres refusing a value its column's
// type cannot hold (invalid_text_representation): an id in a request that
// is not a UUID, which names no row and is the client's mistake, not ours.
func IsBadID(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "22P02"
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
