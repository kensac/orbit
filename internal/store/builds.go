package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TriggerBuild is a build run's trigger: a run of the build agent that
// implements a ticket's approved plan in a sandbox (docs/tickets.md).
const TriggerBuild = "build"

// What a build run is for.
const (
	// BuildPurposeBuild implements the approved plan from the default
	// branch's head.
	BuildPurposeBuild = "build"
	// BuildPurposeFix makes failing checks pass on top of the PR's head,
	// without changing the plan.
	BuildPurposeFix = "fix"
)

// FromPRHead reports whether a build of purpose starts from the PR's head
// (a fix, a revise) rather than the default branch.
func FromPRHead(purpose string) bool {
	return purpose == BuildPurposeFix || purpose == BuildPurposeRevise
}

// ErrBusy is a ticket that cannot start its build or merge because another
// ticket of the same repository is building or merging
// (tickets_one_building, tickets_one_merging). It waits; nothing failed.
var ErrBusy = errors.New("another ticket of the repository is building or merging")

// StartBuild is what enqueueing a build run needs.
type StartBuild struct {
	TicketID string
	// Agent and SpecSHA are the build agent's.
	Agent   string
	SpecSHA string
	Purpose string
	// Failure is what a fix is told: the failing checks and their log
	// tails, as code collected them.
	Failure json.RawMessage
}

// StartBuild enqueues a build run and moves its ticket to building in one
// transaction: from approved for a build, from review for a fix or a
// revise. A build and a revise are given every review comment submitted
// and not yet delivered (a revise needs one); a fix only fixes. The run is
// in the ticket's conversation when the build agent is the ticket's agent,
// and in the build agent's default conversation otherwise (a run is always
// in a conversation of its own agent). ErrBusy means another ticket of the
// repository is building; ErrConflict that the ticket is not where the
// purpose starts.
func (s *Store) StartBuild(ctx context.Context, b StartBuild) (string, error) {
	from := map[string]string{BuildPurposeBuild: TicketApproved, BuildPurposeFix: TicketReview, BuildPurposeRevise: TicketReview}[b.Purpose]
	if from == "" {
		return "", fmt.Errorf("%w: build purpose %q", ErrInvalid, b.Purpose)
	}
	if (b.Purpose == BuildPurposeFix) != (len(b.Failure) > 0) {
		return "", fmt.Errorf("%w: a fix, and only a fix, carries a failure report", ErrInvalid)
	}
	var runID string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var kind, status, agent, conv string
		var plan *int
		err := tx.QueryRow(ctx, `SELECT kind, status, agent, conversation_id::text, approved_plan FROM tickets WHERE id = $1 FOR UPDATE`,
			b.TicketID).Scan(&kind, &status, &agent, &conv, &plan)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if kind != "request" || status != from || plan == nil {
			return fmt.Errorf("ticket %s is %s: a %s starts from %s: %w", b.TicketID, status, b.Purpose, from, ErrConflict)
		}
		if agent != b.Agent {
			conv = ""
		}
		err = tx.QueryRow(ctx, `INSERT INTO runs (agent, spec_sha, trigger, conversation_id)
			VALUES ($1, $2, 'build', NULLIF($3, '')::uuid) RETURNING id::text`, b.Agent, b.SpecSHA, conv).Scan(&runID)
		if isForeignKeyViolation(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ticket_builds (run_id, ticket_id, purpose, plan_version, failure)
			VALUES ($1, $2, $3, $4, $5)`, runID, b.TicketID, b.Purpose, *plan, nullJSON(b.Failure)); err != nil {
			return err
		}
		if b.Purpose != BuildPurposeFix {
			n, err := giveComments(ctx, tx, b.TicketID, runID)
			if err != nil {
				return err
			}
			if n == 0 && b.Purpose == BuildPurposeRevise {
				return fmt.Errorf("ticket %s has no review comment waiting: %w", b.TicketID, ErrConflict)
			}
		}
		err = setStatus(ctx, tx, b.TicketID, from, TicketBuilding, "orbit", TicketUpdate{},
			map[string]any{"run": runID, "purpose": b.Purpose})
		if isUniqueViolation(err) {
			return ErrBusy
		}
		return err
	})
	return runID, err
}

// Build is one build run of a ticket, with what it builds.
type Build struct {
	RunID       string
	TicketID    string
	Purpose     string
	PlanVersion int
	// Plan is the approved plan's text.
	Plan      string
	Failure   json.RawMessage
	CreatedAt time.Time
	RunStatus string
	RunError  *string
}

const buildSelect = `SELECT b.run_id::text, b.ticket_id::text, b.purpose, b.plan_version, p.body, b.failure, b.created_at,
	r.status, r.error
	FROM ticket_builds b
	JOIN ticket_plans p ON p.ticket_id = b.ticket_id AND p.version = b.plan_version
	JOIN runs r ON r.id = b.run_id `

func collectBuild(rows pgx.Rows) (*Build, error) {
	b, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Build])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &b, err
}

// BuildOfRun returns the build a run is, or nil when the run is not one.
func (s *Store) BuildOfRun(ctx context.Context, runID string) (*Build, error) {
	rows, err := s.Pool.Query(ctx, buildSelect+`WHERE b.run_id = $1`, runID)
	if err != nil {
		return nil, err
	}
	return collectBuild(rows)
}

// LatestBuild returns a ticket's newest build run, or nil when it has none.
func (s *Store) LatestBuild(ctx context.Context, ticketID string) (*Build, error) {
	rows, err := s.Pool.Query(ctx, buildSelect+`WHERE b.ticket_id = $1 ORDER BY b.created_at DESC LIMIT 1`, ticketID)
	if err != nil {
		return nil, err
	}
	return collectBuild(rows)
}

// FixesSinceBuild counts the fix runs since the ticket's newest build or
// revise run: the attempts spent on the checks of what a person last asked
// for.
func (s *Store) FixesSinceBuild(ctx context.Context, ticketID string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM ticket_builds
		WHERE ticket_id = $1 AND purpose = 'fix'
		  AND created_at > COALESCE((SELECT max(created_at) FROM ticket_builds WHERE ticket_id = $1 AND purpose IN ('build', 'revise')), '-infinity')`,
		ticketID).Scan(&n)
	return n, err
}

// latestBuildIs locks the ticket a build run builds and reports its status
// and whether this run is its newest build: only that one may move it.
func latestBuildIs(ctx context.Context, tx pgx.Tx, runID string) (ticketID, status string, newest bool, err error) {
	err = tx.QueryRow(ctx, `
		SELECT t.id::text, t.status,
		       b.run_id = (SELECT n.run_id FROM ticket_builds n WHERE n.ticket_id = t.id ORDER BY n.created_at DESC LIMIT 1)
		FROM ticket_builds b JOIN tickets t ON t.id = b.ticket_id
		WHERE b.run_id = $1 FOR UPDATE OF t`, runID).Scan(&ticketID, &status, &newest)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return ticketID, status, newest, err
}

// FinishBuild records a build run that delivered its PR, and moves its
// ticket from building to review with the branch, PR and head, in one
// transaction. A ticket that moved on meanwhile (abandoned) keeps its state;
// the run is still recorded as what it did. A run with r.Gate set delivered
// without calling the model (an earlier attempt's pushed commit) and is
// skipped, not succeeded.
func (s *Store) FinishBuild(ctx context.Context, runID string, r RunResult, u TicketUpdate) error {
	r.Status = StatusSucceeded
	if r.Gate != "" {
		r.Status = StatusSkipped
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		id, status, newest, err := latestBuildIs(ctx, tx, runID)
		if err != nil {
			return err
		}
		if err := finishRun(ctx, tx, runID, r); err != nil {
			return err
		}
		if status != TicketBuilding || !newest {
			return nil
		}
		u.Error = ""
		return setStatus(ctx, tx, id, TicketBuilding, TicketReview, "orbit", u, map[string]any{"run": runID})
	})
}

// FailBuild records a failed build run and fails its ticket with the run's
// error, in one transaction.
func (s *Store) FailBuild(ctx context.Context, runID string, r RunResult) error {
	r.Status = "failed"
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		id, status, newest, err := latestBuildIs(ctx, tx, runID)
		if err != nil {
			return err
		}
		if err := finishRun(ctx, tx, runID, r); err != nil {
			return err
		}
		if err := releaseMessages(ctx, tx, runID); err != nil {
			return err
		}
		if status != TicketBuilding || !newest {
			return nil
		}
		return setStatus(ctx, tx, id, TicketBuilding, TicketFailed, "orbit", TicketUpdate{Error: "build: " + r.Error},
			map[string]any{"run": runID})
	})
}

// Decision is one answer orbit's permission handler gave a build's CLI.
type Decision struct {
	RunID     string          `json:"run_id"`
	Attempt   int             `json:"attempt"`
	RequestID string          `json:"request_id"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	Allowed   bool            `json:"allowed"`
	Message   string          `json:"message,omitempty"`
	At        time.Time       `json:"at"`
}

// RecordDecision stores one permission decision. The handler writes it
// before it answers, and denies when this fails, so no allow is ever
// unrecorded.
// Like every write of a running attempt it is fenced by the attempt: one
// that lost its claim records nothing, so it allows nothing.
func (s *Store) RecordDecision(ctx context.Context, d Decision) error {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO build_decisions (run_id, attempt, request_id, tool, input, allowed, message)
		SELECT $1, $2, $3, $4, $5, $6, NULLIF($7, '') FROM runs WHERE id = $1 AND status = 'running' AND attempt = $2`,
		d.RunID, d.Attempt, d.RequestID, d.Tool, []byte(d.Input), d.Allowed, d.Message)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("decision for run %s attempt %d: %w (not its running claim)", d.RunID, d.Attempt, ErrConflict)
	}
	return nil
}

// RecordSandbox records a sandbox a running build attempt created, fenced
// like RecordDecision: an attempt that lost its claim gets ErrConflict and
// destroys the sandbox it just made.
func (s *Store) RecordSandbox(ctx context.Context, runID string, attempt int, sandboxID string) error {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO build_sandboxes (sandbox_id, run_id, attempt)
		SELECT $3, $1, $2 FROM runs WHERE id = $1 AND status = 'running' AND attempt = $2`, runID, attempt, sandboxID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("sandbox for run %s attempt %d: %w (not its running claim)", runID, attempt, ErrConflict)
	}
	return nil
}

// SandboxDestroyed marks a sandbox gone.
func (s *Store) SandboxDestroyed(ctx context.Context, sandboxID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE build_sandboxes SET destroyed_at = now() WHERE sandbox_id = $1 AND destroyed_at IS NULL`, sandboxID)
	return err
}

// OpenSandboxes returns the sandboxes a run's attempts created and did not
// see destroyed: what an interrupted attempt left behind.
func (s *Store) OpenSandboxes(ctx context.Context, runID string) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT sandbox_id FROM build_sandboxes WHERE run_id = $1 AND destroyed_at IS NULL ORDER BY created_at`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Decisions returns a run's permission decisions in order.
func (s *Store) Decisions(ctx context.Context, runID string) ([]Decision, error) {
	rows, err := s.Pool.Query(ctx, `SELECT run_id::text, attempt, request_id, tool, input, allowed, COALESCE(message, ''), at
		FROM build_decisions WHERE run_id = $1 ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Decision])
}

// MergeProposal is the github:merge action the ticket loop proposes once a
// PR's checks are green.
type MergeProposal struct {
	TicketID string
	// RunID and Agent are the build run that delivered the PR and its
	// agent: an action belongs to a run.
	RunID   string
	Agent   string
	HeadSHA string
	Params  json.RawMessage
	Reason  string
	// Rule is the build agent's rule for github:merge (approve or auto).
	Rule string
}

// ProposeMerge stores the merge action and moves the ticket from review to
// merging, in one transaction, only while the ticket's head is still the
// commit whose checks passed. ErrBusy means another ticket of the
// repository is merging (or merged and not yet deployed).
func (s *Store) ProposeMerge(ctx context.Context, m MergeProposal) (string, error) {
	if m.Rule != "approve" && m.Rule != "auto" {
		return "", fmt.Errorf("%w: a merge is proposed with rule approve or auto, not %q", ErrInvalid, m.Rule)
	}
	var id string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var status string
		var head *string
		err := tx.QueryRow(ctx, `SELECT status, head_sha FROM tickets WHERE id = $1 FOR UPDATE`, m.TicketID).Scan(&status, &head)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if status != TicketReview || head == nil || *head != m.HeadSHA {
			return fmt.Errorf("ticket %s is %s at %v, not in review at %s: %w", m.TicketID, status, head, m.HeadSHA, ErrConflict)
		}
		if id, err = insertAction(ctx, tx, m.RunID, m.Agent, ProposedAction{Type: "github:merge", Params: m.Params,
			Reason: m.Reason, Rule: m.Rule}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tickets SET merge_action_id = $2 WHERE id = $1`, m.TicketID, id); err != nil {
			return err
		}
		err = setStatus(ctx, tx, m.TicketID, TicketReview, TicketMerging, "orbit", TicketUpdate{},
			map[string]any{"action": id, "rule": m.Rule})
		if isUniqueViolation(err) {
			return ErrBusy
		}
		return err
	})
	return id, err
}

// failedWatch and abandonedWatch bound how long the loop keeps looking at a
// failed ticket that reached merging (its PR may yet turn out merged) and an abandoned one
// with a PR (to close it): long enough to outlast an outage, short enough
// that the loop's reads stay a handful per tick.
const (
	failedWatch    = 7 * 24 * time.Hour
	abandonedWatch = 24 * time.Hour
)

// TicketsInFlight returns the requests the ticket loop reconciles, oldest
// first: every state between approved and done, failed tickets that reached
// merging recently or wait on a person's rerun, and recently abandoned
// tickets with a PR.
func (s *Store) TicketsInFlight(ctx context.Context) ([]Ticket, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+ticketCols+` FROM tickets
		WHERE kind = 'request' AND (
		    status IN ('approved', 'building', 'review', 'merging', 'merged')
		    OR (status = 'failed' AND merge_action_id IS NOT NULL AND updated_at > now() - $1::interval)
		    OR (status = 'failed' AND EXISTS (SELECT 1 FROM ticket_check_reruns r WHERE r.ticket_id = tickets.id AND r.done_at IS NULL))
		    OR (status = 'abandoned' AND pr_number IS NOT NULL AND updated_at > now() - $2::interval))
		ORDER BY created_at`, failedWatch.String(), abandonedWatch.String())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Ticket])
}

// ticketLoopLock is the advisory lock the ticket loop runs under: "orbit-tk"
// in ASCII. During a deploy two instances run, and one loop at a time keeps
// their GitHub reads and proposals from doubling (the guarded transitions
// would refuse the second anyway).
const ticketLoopLock = 0x6f72626974746b

// WithTicketLoopLock runs fn while holding the ticket loop's lock, or
// returns false without running it when another instance holds it.
func (s *Store) WithTicketLoopLock(ctx context.Context, fn func() error) (bool, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(ticketLoopLock)).Scan(&ok); err != nil || !ok {
		return false, err
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(ticketLoopLock))
	}()
	return true, fn()
}
