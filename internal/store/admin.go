package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// The admin read side: usage totals and the deployment's health. Every
// query here reads; none takes a lock a run needs.

// ---------------------------------------------------------------- usage

// RunStatuses is every run status, so a count by status lists them all.
var RunStatuses = []string{"queued", "running", "succeeded", "failed", "cancelled", "skipped"}

// CostTotal is a sum of runs' API-equivalent cost and how many runs it covers.
// The sum is exact (numeric in Postgres) and kept as its decimal literal.
type CostTotal struct {
	APIEquivalentUSD json.Number    `json:"api_equivalent_usd"`
	Runs             int            `json:"runs"`
	RunsWithCost     int            `json:"runs_with_cost"`
	ByStatus         map[string]int `json:"by_status"`
}

// AgentCost is CostTotal for one agent.
type AgentCost struct {
	Agent string `json:"agent"`
	CostTotal
}

// DayCost is the cost and run count of one calendar day.
type DayCost struct {
	Date             string      `json:"date"`
	APIEquivalentUSD json.Number `json:"api_equivalent_usd"`
	Runs             int         `json:"runs"`
}

// CostReport is runs created on the days from..to (inclusive, in tz).
type CostReport struct {
	Total   CostTotal   `json:"total"`
	ByAgent []AgentCost `json:"by_agent"`
	ByDay   []DayCost   `json:"by_day"`
}

// MaxUsageDays bounds a usage range, so by_day stays a page, not a table.
const MaxUsageDays = 366

// Costs totals runs created on the calendar days from..to (YYYY-MM-DD,
// inclusive) in the IANA zone tz: overall, per agent, per day (every day
// listed, zero or not). The caller bounds the range (MaxUsageDays). One
// read-only snapshot, so the three views agree.
func (s *Store) Costs(ctx context.Context, from, to, tz string) (*CostReport, error) {
	rep := &CostReport{ByAgent: []AgentCost{}, ByDay: []DayCost{}}
	// The range is [from 00:00, to+1 00:00) in tz; Postgres resolves each
	// local midnight to an instant, DST included.
	const inRange = `created_at >= ($1::date)::timestamp AT TIME ZONE $3
		AND created_at < ($2::date + 1)::timestamp AT TIME ZONE $3`
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT GROUPING(agent) = 1, GROUPING(status) = 1, coalesce(agent, ''), coalesce(status, ''),
			       round(coalesce(sum(cost_usd), 0), 6)::text, count(*), count(cost_usd)
			FROM runs WHERE `+inRange+`
			GROUP BY GROUPING SETS ((agent, status), (agent), (status), ())
			ORDER BY 3, 4`, from, to, tz)
		if err != nil {
			return err
		}
		agents := map[string]*AgentCost{}
		var order []string
		rep.Total = newCostTotal()
		for rows.Next() {
			var allAgents, allStatuses bool
			var agent, status, sum string
			var n, withCost int
			if err := rows.Scan(&allAgents, &allStatuses, &agent, &status, &sum, &n, &withCost); err != nil {
				rows.Close()
				return err
			}
			switch {
			case allAgents && allStatuses:
				rep.Total.APIEquivalentUSD, rep.Total.Runs, rep.Total.RunsWithCost = json.Number(sum), n, withCost
			case allAgents:
				rep.Total.ByStatus[status] = n
			default:
				a, ok := agents[agent]
				if !ok {
					a = &AgentCost{Agent: agent, CostTotal: newCostTotal()}
					agents[agent] = a
					order = append(order, agent)
				}
				if allStatuses {
					a.APIEquivalentUSD, a.Runs, a.RunsWithCost = json.Number(sum), n, withCost
				} else {
					a.ByStatus[status] = n
				}
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, name := range order {
			rep.ByAgent = append(rep.ByAgent, *agents[name])
		}

		rows, err = tx.Query(ctx, `
			SELECT d::date::text, round(coalesce(sum(r.cost_usd), 0), 6)::text, count(r.id)
			FROM generate_series($1::date, $2::date, interval '1 day') d
			LEFT JOIN (SELECT id, cost_usd, (created_at AT TIME ZONE $3)::date AS day
			           FROM runs WHERE `+inRange+`) r ON r.day = d::date
			GROUP BY d ORDER BY d`, from, to, tz)
		if err != nil {
			return err
		}
		days, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (DayCost, error) {
			var d DayCost
			var sum string
			err := row.Scan(&d.Date, &sum, &d.Runs)
			d.APIEquivalentUSD = json.Number(sum)
			return d, err
		})
		rep.ByDay = append(rep.ByDay, days...)
		return err
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

func newCostTotal() CostTotal {
	t := CostTotal{APIEquivalentUSD: "0.000000", ByStatus: map[string]int{}}
	for _, s := range RunStatuses {
		t.ByStatus[s] = 0
	}
	return t
}

// RateLimitEvent is one stored Claude Code rate_limit_event line.
type RateLimitEvent struct {
	RunID   string
	At      time.Time
	Payload json.RawMessage
}

// RateLimitLookback is how far back RateLimitEvents looks: the longest
// window Claude Code reports (seven_day) and a day. An observation older
// than that describes a window that has since reset.
const RateLimitLookback = 8 * 24 * time.Hour

// maxRateLimitEvents bounds the read. The CLI emits one when its rate limit
// information changes (at most a few per run), so this is many runs' worth.
const maxRateLimitEvents = 1000

// RateLimitEvents returns the newest rate_limit_event lines from runs
// created within RateLimitLookback, newest first.
func (s *Store) RateLimitEvents(ctx context.Context) ([]RateLimitEvent, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT e.run_id::text, e.at, e.payload
		FROM runs r JOIN run_events e ON e.run_id = r.id
		WHERE r.created_at > now() - $1::interval AND e.kind = 'rate_limit_event'
		ORDER BY e.at DESC, e.run_id, e.attempt DESC, e.seq DESC
		LIMIT $2`, RateLimitLookback.String(), maxRateLimitEvents)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[RateLimitEvent])
}

// ---------------------------------------------------------------- health

// QueueDepth counts queued runs: due ones an executor may claim now, and
// continuations waiting for their run_after.
type QueueDepth struct {
	Due                  int        `json:"due"`
	WaitingContinuations int        `json:"waiting_continuations"`
	OldestDueAt          *time.Time `json:"oldest_due_at"`
}

// RunningRun is a run an executor holds.
type RunningRun struct {
	RunID       string     `json:"run_id"`
	Agent       string     `json:"agent"`
	Worker      *string    `json:"worker"`
	Attempt     int        `json:"attempt"`
	StartedAt   *time.Time `json:"started_at"`
	HeartbeatAt *time.Time `json:"heartbeat_at"`
	// HeartbeatAgeSeconds is now minus HeartbeatAt, by the database clock.
	HeartbeatAgeSeconds *float64 `json:"heartbeat_age_seconds"`
}

// LastSuccess is an agent's newest run that finished well: succeeded, or
// skipped by a gate (a quiet day is not a stale agent).
type LastSuccess struct {
	RunID      string    `json:"run_id"`
	FinishedAt time.Time `json:"finished_at"`
}

// Health is the queue and run state an operator checks.
type Health struct {
	CheckedAt      time.Time               `json:"checked_at"`
	Queue          QueueDepth              `json:"queue"`
	Running        []RunningRun            `json:"running"`
	PendingActions int                     `json:"pending_actions"`
	LastSuccess    map[string]*LastSuccess `json:"-"`
}

// maxRunning bounds the running list. runs_one_running_lane allows one per
// agent (per conversation for a chat agent), so this is only reached by a
// deployment that busy.
const maxRunning = 200

// Health reads the queue, the running runs, pending actions and each
// present agent's last success, in one read-only snapshot.
func (s *Store) Health(ctx context.Context) (*Health, error) {
	h := &Health{Running: []RunningRun{}, LastSuccess: map[string]*LastSuccess{}}
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT now(),
			       count(*) FILTER (WHERE run_after IS NULL OR run_after <= now()),
			       count(*) FILTER (WHERE run_after > now()),
			       min(created_at) FILTER (WHERE run_after IS NULL OR run_after <= now())
			FROM runs WHERE status = 'queued'`).
			Scan(&h.CheckedAt, &h.Queue.Due, &h.Queue.WaitingContinuations, &h.Queue.OldestDueAt); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT id::text, agent, worker, attempt, started_at, heartbeat_at,
			       extract(epoch FROM now() - heartbeat_at)::float8
			FROM runs WHERE status = 'running' ORDER BY started_at, id LIMIT $1`, maxRunning)
		if err != nil {
			return err
		}
		running, err := pgx.CollectRows(rows, pgx.RowToStructByPos[RunningRun])
		if err != nil {
			return err
		}
		h.Running = append(h.Running, running...)
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM actions WHERE status = 'pending'`).Scan(&h.PendingActions); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			SELECT a.name, ls.id, ls.finished_at
			FROM agents a
			JOIN LATERAL (
			    SELECT r.id::text, r.finished_at FROM runs r
			    WHERE r.agent = a.name AND r.status IN ('succeeded', 'skipped')
			    ORDER BY r.finished_at DESC LIMIT 1
			) ls ON true
			WHERE a.present`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var ls LastSuccess
			if err := rows.Scan(&name, &ls.RunID, &ls.FinishedAt); err != nil {
				return err
			}
			h.LastSuccess[name] = &ls
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return h, nil
}
