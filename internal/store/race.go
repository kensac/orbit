package store

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Race bounds (docs/tickets.md, Races): each lane is a whole build of the
// plan, so three is where the comparison stops paying for itself; one is
// not a race.
const (
	MinRace = 2
	MaxRace = 3
)

// PickType is the action a person's pick is recorded as: rule approve,
// decided and executed by that person in the pick's transaction. No agent
// is offered it and no rule makes it automatic, so only a person picks.
const PickType = "ticket:pick"

// Lanes names the lanes of a race, in order.
var Lanes = []string{"a", "b", "c"}

// Efforts are the CLI's effort levels (--effort; the Agent SDK's
// EffortLevel).
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// modelRE bounds a variant's model to the shapes the CLI takes (an alias
// such as opus, or a full id such as claude-opus-4-1[1m]); it goes into
// the CLI's argv, never through a shell.
var modelRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-]{0,62}(\[[a-z0-9]{1,8}\])?$`)

// Variant is what one lane's builds run with; empty fields keep the build
// agent's runner's.
type Variant struct {
	Model  string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// checkRace validates a ticket's race before anything is written.
func checkRace(t NewTicket) error {
	if t.Race == 0 {
		if len(t.Variants) > 0 {
			return fmt.Errorf("%w: variants need a race", ErrInvalid)
		}
		return nil
	}
	switch {
	case t.Kind != "request":
		return fmt.Errorf("%w: only a request races", ErrInvalid)
	case t.Race < MinRace || t.Race > MaxRace:
		return fmt.Errorf("%w: race is %d to %d builds", ErrInvalid, MinRace, MaxRace)
	case len(t.Variants) > t.Race:
		return fmt.Errorf("%w: %d variants for %d lanes", ErrInvalid, len(t.Variants), t.Race)
	case t.StackOn != "":
		// A stacked ticket follows one branch below it; a race has
		// several until a person picks.
		return fmt.Errorf("%w: a race cannot be stacked", ErrInvalid)
	}
	for i, v := range t.Variants {
		if v.Model != "" && !modelRE.MatchString(v.Model) {
			return fmt.Errorf("%w: variants[%d].model %q is not a model name", ErrInvalid, i, v.Model)
		}
		if v.Effort != "" && !slices.Contains(Efforts, v.Effort) {
			return fmt.Errorf("%w: variants[%d].effort must be one of %v", ErrInvalid, i, Efforts)
		}
	}
	return nil
}

func insertLanes(ctx context.Context, tx pgx.Tx, ticketID string, race int, variants []Variant) error {
	for i := range race {
		var v Variant
		if i < len(variants) {
			v = variants[i]
		}
		b, _ := json.Marshal(v)
		if _, err := tx.Exec(ctx, `INSERT INTO ticket_lanes (ticket_id, lane, variant) VALUES ($1, $2, $3)`, ticketID, Lanes[i], b); err != nil {
			return err
		}
	}
	return nil
}

// StartRace enqueues one build run per lane of a race no person has
// picked from, and moves the ticket from approved to building, in one
// transaction. The runs are the build agent's, so they run one at a time
// (runs_one_running), each from the default branch's head when it starts.
// ErrBusy means another ticket of the repository is building.
func (s *Store) StartRace(ctx context.Context, b StartBuild) ([]string, error) {
	var runs []string
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var kind, status, agent, conv string
		var plan, race *int
		var picked *string
		err := tx.QueryRow(ctx, `SELECT kind, status, agent, conversation_id::text, approved_plan, race, picked_lane
			FROM tickets WHERE id = $1 FOR UPDATE`, b.TicketID).Scan(&kind, &status, &agent, &conv, &plan, &race, &picked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if kind != "request" || status != TicketApproved || plan == nil || race == nil || picked != nil {
			return fmt.Errorf("ticket %s is %s: a race starts from an approved race no one picked from: %w", b.TicketID, status, ErrConflict)
		}
		if agent != b.Agent {
			conv = ""
		}
		for _, lane := range Lanes[:*race] {
			var id string
			err := tx.QueryRow(ctx, `INSERT INTO runs (agent, spec_sha, trigger, conversation_id)
				VALUES ($1, $2, 'build', NULLIF($3, '')::uuid) RETURNING id::text`, b.Agent, b.SpecSHA, conv).Scan(&id)
			if isForeignKeyViolation(err) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO ticket_builds (run_id, ticket_id, purpose, plan_version, lane)
				VALUES ($1, $2, 'build', $3, $4)`, id, b.TicketID, *plan, lane); err != nil {
				return err
			}
			runs = append(runs, id)
		}
		err = setStatus(ctx, tx, b.TicketID, TicketApproved, TicketBuilding, "orbit", TicketUpdate{},
			map[string]any{"runs": runs, "purpose": BuildPurposeBuild, "race": *race})
		if isUniqueViolation(err) {
			return ErrBusy
		}
		return err
	})
	return runs, err
}

// settleRace moves a racing ticket on once every lane's newest build has
// ended: to review when at least one delivered a PR, else to failed with
// each lane's error. Until then, and once the ticket left building, it
// does nothing. The caller holds the ticket's lock.
func settleRace(ctx context.Context, tx pgx.Tx, id, status string) error {
	if status != TicketBuilding {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (b.lane) b.lane, b.run_id::text, r.status, r.error, b.pr_number
		FROM ticket_builds b JOIN runs r ON r.id = b.run_id
		WHERE b.ticket_id = $1 AND b.lane IS NOT NULL ORDER BY b.lane, b.created_at DESC`, id)
	if err != nil {
		return err
	}
	type lane struct {
		lane, run, status string
		err               *string
		pr                *int
	}
	lanes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (lane, error) {
		var l lane
		return l, r.Scan(&l.lane, &l.run, &l.status, &l.err, &l.pr)
	})
	if err != nil {
		return err
	}
	var delivered, failed []string
	for _, l := range lanes {
		switch {
		case l.status == "queued" || l.status == "running":
			return nil
		case (l.status == StatusSucceeded || l.status == StatusSkipped) && l.pr != nil:
			delivered = append(delivered, l.lane)
		default:
			why := l.status
			if l.err != nil {
				why += ": " + *l.err
			}
			failed = append(failed, l.lane+" "+why)
		}
	}
	if len(lanes) == 0 {
		return nil
	}
	detail := map[string]any{"race": map[string]any{"delivered": delivered, "failed": failed}}
	if len(delivered) == 0 {
		return setStatus(ctx, tx, id, TicketBuilding, TicketFailed, "orbit",
			TicketUpdate{Error: "every race build failed: " + strings.Join(failed, "; ")}, detail)
	}
	return setStatus(ctx, tx, id, TicketBuilding, TicketReview, "orbit", TicketUpdate{}, detail)
}

// SettleRace is settleRace for the ticket loop: a lane run the reaper
// failed, or a person cancelled, never reached FinishBuild or FailBuild.
func (s *Store) SettleRace(ctx context.Context, id string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		_, status, err := lockTicket(ctx, tx, id)
		if err != nil {
			return err
		}
		return settleRace(ctx, tx, id, status)
	})
}

// RecordLaneChecks stores the loop's reading of a lane PR before the pick:
// its state and its required checks at head.
func (s *Store) RecordLaneChecks(ctx context.Context, ticketID, lane, head, checks, prState string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE ticket_lanes SET checks = NULLIF($3, ''), checks_head = $4, pr_state = $5, checks_at = now()
		WHERE ticket_id = $1 AND lane = $2`, ticketID, lane, checks, head, prState)
	return err
}

// LaneClosed records that a lane PR nobody picked is no longer open: the
// loop closed it (closed), or it was closed or merged on GitHub. Its
// branch is kept.
func (s *Store) LaneClosed(ctx context.Context, ticketID, lane, prState string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE ticket_lanes SET pr_closed_at = now(), pr_state = $3
		WHERE ticket_id = $1 AND lane = $2 AND pr_closed_at IS NULL`, ticketID, lane, prState)
	return err
}

// Lane is one lane of a race as the ticket review compares it: its
// variant, its newest build, and what code scored from them.
type Lane struct {
	Lane    string  `json:"lane"`
	Variant Variant `json:"variant"`
	// From the lane's newest build run.
	RunID      *string  `json:"run_id,omitempty"`
	RunStatus  *string  `json:"run_status,omitempty"`
	Error      *string  `json:"error,omitempty"`
	Branch     *string  `json:"branch,omitempty"`
	PRNumber   *int     `json:"pr_number,omitempty"`
	BaseSHA    *string  `json:"base_sha,omitempty"`
	HeadSHA    *string  `json:"head_sha,omitempty"`
	Added      *int     `json:"added,omitempty"`
	Deleted    *int     `json:"deleted,omitempty"`
	Files      []string `json:"files"`
	DiffPolicy *string  `json:"diff_policy,omitempty"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
	// The loop's latest reading of its PR.
	Checks     *string    `json:"checks,omitempty"`
	ChecksHead *string    `json:"checks_head,omitempty"`
	PRState    *string    `json:"pr_state,omitempty"`
	ChecksAt   *time.Time `json:"checks_at,omitempty"`
	PRClosedAt *time.Time `json:"pr_closed_at,omitempty"`
	// Scored by RankLanes.
	Delivered    bool   `json:"delivered"`
	ChecksGreen  bool   `json:"checks_green"`
	LinesChanged int    `json:"lines_changed"`
	TestsTouched int    `json:"tests_touched"`
	Rank         int    `json:"rank"`
	Picked       bool   `json:"picked"`
	Why          string `json:"why"`
}

// RaceLanes returns a race's lanes, scored and ranked (RankLanes), in lane
// order, and the lane a person picked.
func (s *Store) RaceLanes(ctx context.Context, ticketID string) ([]Lane, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT l.lane, l.variant, b.run_id::text, r.status, r.error, b.branch, b.pr_number, b.base_sha, b.head_sha,
		       b.added, b.deleted, COALESCE(b.files, '[]'), b.diff_policy, r.cost_usd,
		       l.checks, l.checks_head, l.pr_state, l.checks_at, l.pr_closed_at, t.picked_lane IS NOT DISTINCT FROM l.lane
		FROM ticket_lanes l JOIN tickets t ON t.id = l.ticket_id
		LEFT JOIN LATERAL (SELECT * FROM ticket_builds n WHERE n.ticket_id = l.ticket_id AND n.lane = l.lane
		                   ORDER BY n.created_at DESC LIMIT 1) b ON true
		LEFT JOIN runs r ON r.id = b.run_id
		WHERE l.ticket_id = $1 ORDER BY l.lane`, ticketID)
	if err != nil {
		return nil, err
	}
	lanes, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Lane, error) {
		var l Lane
		var variant, files []byte
		err := r.Scan(&l.Lane, &variant, &l.RunID, &l.RunStatus, &l.Error, &l.Branch, &l.PRNumber, &l.BaseSHA, &l.HeadSHA,
			&l.Added, &l.Deleted, &files, &l.DiffPolicy, &l.CostUSD,
			&l.Checks, &l.ChecksHead, &l.PRState, &l.ChecksAt, &l.PRClosedAt, &l.Picked)
		if err == nil {
			err = errors.Join(json.Unmarshal(variant, &l.Variant), json.Unmarshal(files, &l.Files))
		}
		return l, err
	})
	if err != nil {
		return nil, err
	}
	RankLanes(lanes)
	return lanes, nil
}

// TestPath reports whether a changed path is a test: Go's _test.go, the
// .test. and .spec. conventions, or anything under a test, tests,
// testdata or __tests__ directory.
func TestPath(p string) bool {
	base := path.Base(p)
	if strings.HasSuffix(base, "_test.go") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	for seg := range strings.SplitSeq(path.Dir(p), "/") {
		switch seg {
		case "test", "tests", "testdata", "__tests__":
			return true
		}
	}
	return false
}

// RankLanes scores each lane and ranks them, best first, in code
// (docs/tickets.md, Races): a lane that delivered a PR (its diff policy
// passed) before one that did not; then the required checks green at the
// PR's head, then pending, then failed or unread; then one that touched
// tests before one that did not; then fewer lines changed; then lower
// cost; then the lane's letter. A closed or merged lane PR ranks after the
// open ones. Rank 1 is the recommendation when it delivered.
func RankLanes(lanes []Lane) {
	checks := func(l *Lane) int {
		switch {
		case l.PRState != nil && *l.PRState != "open":
			return 3
		case l.Checks == nil || l.ChecksHead == nil || l.HeadSHA == nil || *l.ChecksHead != *l.HeadSHA:
			return 2 // not read at this head yet
		case *l.Checks == "green":
			return 0
		case *l.Checks == "pending":
			return 1
		}
		return 2
	}
	for i := range lanes {
		l := &lanes[i]
		l.Delivered = l.RunStatus != nil && (*l.RunStatus == StatusSucceeded || *l.RunStatus == StatusSkipped) && l.PRNumber != nil
		l.ChecksGreen = l.Delivered && checks(l) == 0
		l.LinesChanged = intOr0(l.Added) + intOr0(l.Deleted)
		l.TestsTouched = 0
		for _, f := range l.Files {
			if TestPath(f) {
				l.TestsTouched++
			}
		}
		if l.Files == nil {
			l.Files = []string{}
		}
		switch {
		case l.RunStatus == nil:
			l.Why = "no build yet"
		case !l.Delivered && l.DiffPolicy != nil && *l.DiffPolicy == "failed":
			l.Why = "the diff policy refused its diff"
		case !l.Delivered && (*l.RunStatus == "queued" || *l.RunStatus == "running"):
			l.Why = "building"
		case !l.Delivered:
			l.Why = "its build " + *l.RunStatus
		default:
			l.Why = [...]string{"checks green", "checks running", "checks failed or not read yet", "its PR is not open"}[checks(l)]
		}
	}
	order := make([]*Lane, len(lanes))
	for i := range lanes {
		order[i] = &lanes[i]
	}
	cost := func(l *Lane) float64 {
		if l.CostUSD == nil {
			return 0
		}
		return *l.CostUSD
	}
	slices.SortStableFunc(order, func(a, b *Lane) int {
		if a.Delivered != b.Delivered {
			if a.Delivered {
				return -1
			}
			return 1
		}
		return cmp.Or(
			cmp.Compare(checks(a), checks(b)),
			cmp.Compare(min(b.TestsTouched, 1), min(a.TestsTouched, 1)),
			cmp.Compare(a.LinesChanged, b.LinesChanged),
			cmp.Compare(cost(a), cost(b)),
			cmp.Compare(a.Lane, b.Lane),
		)
	})
	for i, l := range order {
		l.Rank = i + 1
	}
}

func intOr0(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

// Pick is a person picking one lane of a race.
type Pick struct {
	TicketID string
	Lane     string
	By       string
	// ClientID makes the pick idempotent (receipts).
	ClientID string
}

// PickLane records a person's pick of one lane of a race in review, in one
// transaction: a ticket:pick action on the lane's build run (rule approve,
// approved and executed by the person), and the ticket taking that lane's
// branch, PR, base and head, so from here it is reviewed, checked, fixed
// and merged like any ticket. The loop closes the other lanes' PRs and
// keeps their branches. ErrConflict means the ticket is not a race in
// review without a pick, or the lane did not deliver a PR.
func (s *Store) PickLane(ctx context.Context, p Pick) (actionID string, replayed bool, err error) {
	if !slices.Contains(Lanes, p.Lane) {
		return "", false, fmt.Errorf("%w: lane must be one of %v", ErrInvalid, Lanes)
	}
	var agent string
	if err := s.Pool.QueryRow(ctx, `SELECT agent FROM tickets WHERE id = $1`, p.TicketID).Scan(&agent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, ErrNotFound
		}
		return "", false, err
	}
	req := map[string]any{"op": "pick", "ticket": p.TicketID, "lane": p.Lane, "by": p.By}
	raw, replayed, err := s.withReceipt(ctx, agent, p.ClientID, req, func(tx pgx.Tx) (json.RawMessage, error) {
		var status string
		var race *int
		var picked *string
		err := tx.QueryRow(ctx, `SELECT status, race, picked_lane FROM tickets WHERE id = $1 FOR UPDATE`, p.TicketID).Scan(&status, &race, &picked)
		if err != nil {
			return nil, err
		}
		if race == nil || picked != nil || status != TicketReview {
			return nil, fmt.Errorf("ticket %s is not a race in review waiting for a pick: %w", p.TicketID, ErrConflict)
		}
		var run, buildAgent, runStatus string
		var branch, base, head *string
		var pr *int
		err = tx.QueryRow(ctx, `SELECT b.run_id::text, r.agent, r.status, b.branch, b.base_sha, b.head_sha, b.pr_number
			FROM ticket_builds b JOIN runs r ON r.id = b.run_id
			WHERE b.ticket_id = $1 AND b.lane = $2 ORDER BY b.created_at DESC LIMIT 1`, p.TicketID, p.Lane).
			Scan(&run, &buildAgent, &runStatus, &branch, &base, &head, &pr)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("ticket %s has no lane %s: %w", p.TicketID, p.Lane, ErrConflict)
		}
		if err != nil {
			return nil, err
		}
		if (runStatus != StatusSucceeded && runStatus != StatusSkipped) || pr == nil || head == nil || branch == nil {
			return nil, fmt.Errorf("lane %s did not deliver a PR (its build %s): %w", p.Lane, runStatus, ErrConflict)
		}
		params, _ := json.Marshal(map[string]any{"ticket_id": p.TicketID, "lane": p.Lane, "pull_number": *pr, "head_sha": *head})
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO actions (run_id, agent, type, params, reason, rule, status, decided_by, decided_at, executed_at, result, created_at)
			VALUES ($1, $2, $3, $4, $5, 'approve', 'executed', $6, now(), now(), $4, clock_timestamp())
			RETURNING id::text`, run, buildAgent, PickType, params, "A person picked lane "+p.Lane+" of the race.", p.By).Scan(&id); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE tickets SET picked_lane = $2 WHERE id = $1`, p.TicketID, p.Lane); err != nil {
			return nil, err
		}
		u := TicketUpdate{Branch: *branch, PRNumber: pr, HeadSHA: *head}
		if base != nil {
			u.BaseSHA = *base
		}
		if err := setStatus(ctx, tx, p.TicketID, TicketReview, TicketReview, p.By, u,
			map[string]any{"picked": p.Lane, "action": id, "pull_number": *pr}); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"action_id": id})
	})
	if err != nil {
		return "", false, err
	}
	var r struct {
		ActionID string `json:"action_id"`
	}
	return r.ActionID, replayed, json.Unmarshal(raw, &r)
}
