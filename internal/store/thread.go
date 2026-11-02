package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kensac/orbit/internal/status"
)

// ThreadItem is one entry of an agent's conversation: exactly one of
// Message and Run is set.
type ThreadItem struct {
	Message *ThreadMessage
	Run     *ThreadRun
}

// MarshalJSON encodes the item as whichever it is.
func (t ThreadItem) MarshalJSON() ([]byte, error) {
	if t.Message != nil {
		return json.Marshal(t.Message)
	}
	return json.Marshal(t.Run)
}

// key is the item's position in the thread's total order: time, then a run
// before a message at the same time, then id.
func (t ThreadItem) key() ThreadCursor {
	if t.Message != nil {
		return ThreadCursor{At: t.Message.At, Rank: rankMessage, ID: t.Message.ID}
	}
	return ThreadCursor{At: t.Run.At, Rank: rankRun, ID: t.Run.ID}
}

const (
	rankRun     = 0
	rankMessage = 1
)

// ThreadMessage is a person's message or an agent's question.
type ThreadMessage struct {
	Kind    string    `json:"kind"`
	ID      string    `json:"id"`
	At      time.Time `json:"at"`
	From    string    `json:"from"`
	Body    string    `json:"body"`
	Author  *string   `json:"author,omitempty"`
	ReplyTo *string   `json:"reply_to,omitempty"`
	// RunID is the run that read it (person) or asked it (agent).
	RunID *string `json:"run_id"`
	// Open is set on a question: true until answered (openQuestion).
	Open *bool `json:"open,omitempty"`
	// Attachments are the files a person's message carries; always an
	// array.
	Attachments []Upload `json:"attachments"`
}

// ThreadRun is a run with what it said: its report, its questions and the
// actions it proposed. A stream's done event is this, too.
type ThreadRun struct {
	Kind       string     `json:"kind"`
	ID         string     `json:"id"`
	At         time.Time  `json:"at"`
	Trigger    string     `json:"trigger"`
	Status     string     `json:"status"`
	Attempt    int        `json:"attempt"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Steps      int        `json:"steps"`
	// Turn is the latest journal turn of the run's latest timeline (a
	// journal.Turn), null before the backend was called.
	Turn      json.RawMessage  `json:"turn"`
	Summary   *string          `json:"summary"`
	Report    *string          `json:"report"`
	Questions []ThreadQuestion `json:"questions"`
	Error     *string          `json:"error"`
	CostUSD   *float64         `json:"cost_usd"`
	Actions   []ThreadAction   `json:"actions"`
	// Gate names what ended a skipped run; null otherwise.
	Gate *string `json:"gate"`
	// Tasks are the tasks the run delegated, in the order it listed them.
	Tasks []ThreadTask `json:"tasks"`
	// NeedsInput is true when the run is waiting on its person: one of its
	// questions is open or one of its actions awaits approval.
	NeedsInput bool `json:"needs_input"`
	// AgentStatus is the agent's status in this run (status.Derive).
	AgentStatus status.Status `json:"agent_status"`
}

// ThreadQuestion is a question a run asked; ID is its from_agent message,
// the id a reply names in reply_to.
type ThreadQuestion struct {
	ID   string `json:"id"`
	Body string `json:"body"`
	Open bool   `json:"open"`
}

// ThreadAction is an action as a thread shows it.
type ThreadAction struct {
	ID     string          `json:"id"`
	Type   string          `json:"type"`
	Status string          `json:"status"`
	Reason string          `json:"reason"`
	Params json.RawMessage `json:"params"`
}

// ThreadTask is a delegated task as its parent's thread shows it; the whole
// task, with its report, is GET /tasks/{id}.
type ThreadTask struct {
	ID             string   `json:"id"`
	Key            string   `json:"key"`
	Agent          string   `json:"agent"`
	Status         string   `json:"status"`
	ConversationID string   `json:"conversation_id"`
	DependsOn      []string `json:"depends_on"`
	Summary        *string  `json:"summary"`
	Error          *string  `json:"error"`
	// DecisionActionID is the task:decision a person resolves before it
	// starts, when it has one.
	DecisionActionID *string `json:"decision_action_id"`
}

// DefaultThreadLimit and MaxThreadLimit are a thread page's size.
const (
	DefaultThreadLimit = 50
	MaxThreadLimit     = 200
)

// runEpoch is the journal epoch of a run's latest attempt (aliased r):
// the timeline its stream shows.
const runEpoch = `orbit_attempt_epoch(r.id, r.attempt)`

// stepsCount counts the tool calls in the timeline of a run's latest
// attempt: exactly the tool events its stream shows (each call's first
// journal revision; TestStepsMatchTheStream holds the two together).
const stepsCount = `(
	SELECT count(*) FROM journal_items j
	WHERE r.attempt > 0 AND j.run_id = r.id AND j.epoch = ` + runEpoch + `
	  AND j.kind = 'tool_call' AND j.revision = 1)`

// latestTurn is the newest revision of the latest turn in that timeline:
// how the backend exchange stands now.
const latestTurn = `(
	SELECT j.body FROM journal_items j
	WHERE r.attempt > 0 AND j.run_id = r.id AND j.epoch = ` + runEpoch + ` AND j.kind = 'turn'
	ORDER BY j.seq DESC LIMIT 1)`

// threadRunSelect reads runs with their questions, actions, steps and turn
// in one round trip; the correlated aggregates use messages_run,
// actions_run and journal_items' key, so a page costs the same queries
// however many runs it holds.
const threadRunSelect = `
	SELECT r.id::text, r.created_at, r.trigger, r.status, r.attempt, r.started_at, r.finished_at,
	       ` + stepsCount + `, ` + latestTurn + `, r.summary, r.report, r.error, r.cost_usd::float8, r.gate,
	       COALESCE((SELECT json_agg(json_build_object('id', m.id, 'body', m.body, 'open', (` + openQuestion + `)) ORDER BY m.created_at, m.id)
	                 FROM messages m WHERE m.run_id = r.id AND m.direction = 'from_agent'), '[]'),
	       COALESCE((SELECT json_agg(json_build_object('id', a.id, 'type', a.type, 'status', a.status,
	                                                   'reason', a.reason, 'params', a.params)
	                                 ORDER BY a.created_at, a.id)
	                 FROM actions a WHERE a.run_id = r.id), '[]'),
	       COALESCE((SELECT json_agg(json_build_object('id', k.id, 'key', k.key, 'agent', k.agent, 'status', k.status,
	                                                   'conversation_id', k.conversation_id, 'summary', k.report->>'summary',
	                                                   'error', k.error, 'decision_action_id', k.decision_action_id,
	                                                   'depends_on', COALESCE((SELECT json_agg(d.key ORDER BY d.key)
	                                                       FROM task_deps x JOIN tasks d ON d.id = x.depends_on
	                                                       WHERE x.task_id = k.id), '[]'))
	                                 ORDER BY k.created_at, k.id)
	                 FROM tasks k WHERE k.parent_run_id = r.id), '[]'),
	       r.hook_status
	FROM runs r`

func scanThreadRun(row pgx.Row) (*ThreadRun, error) {
	r := ThreadRun{Kind: "run"}
	var questions, actions, tasks, hook []byte
	if err := row.Scan(&r.ID, &r.At, &r.Trigger, &r.Status, &r.Attempt, &r.StartedAt, &r.FinishedAt, &r.Steps,
		&r.Turn, &r.Summary, &r.Report, &r.Error, &r.CostUSD, &r.Gate, &questions, &actions, &tasks, &hook); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal(questions, &r.Questions); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(actions, &r.Actions); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(tasks, &r.Tasks); err != nil {
		return nil, err
	}
	openQ, pending := false, false
	for _, q := range r.Questions {
		openQ = openQ || q.Open
	}
	for _, a := range r.Actions {
		pending = pending || a.Status == "pending"
	}
	r.NeedsInput = openQ || pending
	r.AgentStatus = status.Derive(status.Run{ID: r.ID, Status: r.Status, Attempt: r.Attempt, CreatedAt: r.At,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Error: r.Error, OpenQuestion: openQ, PendingAction: pending,
		Hook: hookSnapshot(hook)})
	return &r, nil
}

// ThreadRunByID returns one run as a thread shows it.
func (s *Store) ThreadRunByID(ctx context.Context, id string) (*ThreadRun, error) {
	return scanThreadRun(s.Pool.QueryRow(ctx, threadRunSelect+` WHERE r.id = $1`, id))
}

// ThreadCursor is a position in a thread. Items are totally ordered by
// (at, rank, id), so paging by it never skips or repeats an item, even when
// several share a timestamp.
type ThreadCursor struct {
	At   time.Time
	Rank int
	ID   string
}

// String encodes the cursor for a client, which treats it as opaque.
func (c ThreadCursor) String() string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(c.At.UTC().Format(time.RFC3339Nano) + "|" + strconv.Itoa(c.Rank) + "|" + c.ID))
}

// ParseThreadCursor decodes String's output.
func ParseThreadCursor(s string) (*ThreadCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	parts := strings.Split(string(b), "|")
	if err != nil || len(parts) != 3 {
		return nil, errors.New("not a cursor from next_before")
	}
	at, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return nil, fmt.Errorf("cursor time: %w", err)
	}
	rank, err := strconv.Atoi(parts[1])
	if err != nil || (rank != rankRun && rank != rankMessage) {
		return nil, errors.New("cursor rank")
	}
	return &ThreadCursor{At: at, Rank: rank, ID: parts[2]}, nil
}

// ThreadPage is a page of a thread, oldest first. HasMore says older items
// exist; NextBefore is the cursor that fetches them.
type ThreadPage struct {
	Items      []ThreadItem
	HasMore    bool
	NextBefore string
}

// Thread returns an agent's default conversation (see ConversationThread).
func (s *Store) Thread(ctx context.Context, agent string, before *ThreadCursor, limit int) (*ThreadPage, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE agent = $1 AND is_default`, agent).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.ConversationThread(ctx, id, before, limit)
}

// ConversationThread returns one conversation's messages (both directions)
// and runs (every trigger): the newest limit items before the cursor (nil:
// the newest), oldest first.
func (s *Store) ConversationThread(ctx context.Context, conversationID string, before *ThreadCursor, limit int) (*ThreadPage, error) {
	limit = clampLimit(limit, DefaultThreadLimit, MaxThreadLimit)
	at, rank, id := any(nil), rankMessage+1, "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if before != nil {
		at, rank, id = before.At, before.Rank, before.ID
	}
	page := &ThreadPage{}
	err := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM conversations WHERE id = $1)`, conversationID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		// The page's keys, one more than limit to learn whether older items
		// exist. Each branch is a range scan on (conversation_id, created_at)
		// (messages_conversation_recent, runs_conversation_recent) bounded
		// by the limit.
		rows, err := tx.Query(ctx, `
			(SELECT created_at, 1, id FROM messages
			 WHERE conversation_id = $1 AND created_at <= COALESCE($2::timestamptz, 'infinity')
			   AND (created_at, 1, id) < (COALESCE($2::timestamptz, 'infinity'), $3::int, $4::uuid)
			 ORDER BY created_at DESC, id DESC LIMIT $5)
			UNION ALL
			(SELECT created_at, 0, id FROM runs
			 WHERE conversation_id = $1 AND created_at <= COALESCE($2::timestamptz, 'infinity')
			   AND (created_at, 0, id) < (COALESCE($2::timestamptz, 'infinity'), $3::int, $4::uuid)
			 ORDER BY created_at DESC, id DESC LIMIT $5)
			ORDER BY 1 DESC, 2 DESC, 3 DESC LIMIT $5`, conversationID, at, rank, id, limit+1)
		if err != nil {
			return err
		}
		var keys []ThreadCursor
		for rows.Next() {
			var k ThreadCursor
			if err := rows.Scan(&k.At, &k.Rank, &k.ID); err != nil {
				rows.Close()
				return err
			}
			keys = append(keys, k)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(keys) > limit {
			keys = keys[:limit]
			page.HasMore = true
			page.NextBefore = keys[limit-1].String()
		}
		var msgIDs, runIDs []string
		for _, k := range keys {
			if k.Rank == rankMessage {
				msgIDs = append(msgIDs, k.ID)
			} else {
				runIDs = append(runIDs, k.ID)
			}
		}
		if len(msgIDs) > 0 {
			rows, err := tx.Query(ctx, `
				SELECT id::text, created_at, direction, body, author, reply_to::text, run_id::text,
				       CASE WHEN direction = 'from_agent' THEN (`+openQuestion+`) END
				FROM messages m WHERE id = ANY($1::uuid[])`, msgIDs)
			if err != nil {
				return err
			}
			for rows.Next() {
				m := ThreadMessage{Kind: "message"}
				var dir string
				if err := rows.Scan(&m.ID, &m.At, &dir, &m.Body, &m.Author, &m.ReplyTo, &m.RunID, &m.Open); err != nil {
					rows.Close()
					return err
				}
				m.From = map[string]string{"to_agent": "person", "from_agent": "agent"}[dir]
				page.Items = append(page.Items, ThreadItem{Message: &m})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return err
			}
			att, err := messageUploads(ctx, tx, msgIDs)
			if err != nil {
				return err
			}
			for _, it := range page.Items {
				if m := it.Message; m != nil {
					m.Attachments = att[m.ID]
					if m.Attachments == nil {
						m.Attachments = []Upload{}
					}
				}
			}
		}
		if len(runIDs) > 0 {
			rows, err := tx.Query(ctx, threadRunSelect+` WHERE r.id = ANY($1::uuid[])`, runIDs)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				r, err := scanThreadRun(rows)
				if err != nil {
					return err
				}
				page.Items = append(page.Items, ThreadItem{Run: r})
			}
			return rows.Err()
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(page.Items, func(i, j int) bool { return page.Items[i].key().less(page.Items[j].key()) })
	return page, nil
}

// less is the thread's order, the same one the query's row comparison
// uses: canonical lowercase UUID text sorts as the UUID's bytes do.
func (c ThreadCursor) less(o ThreadCursor) bool {
	if !c.At.Equal(o.At) {
		return c.At.Before(o.At)
	}
	if c.Rank != o.Rank {
		return c.Rank < o.Rank
	}
	return c.ID < o.ID
}

// RunProgress is what a stream polls: where the run is, and the agent's
// status in it.
type RunProgress struct {
	Status      string
	Attempt     int
	RunnerKind  string
	AgentStatus status.Status
}

// GetRunProgress returns a run's status, attempt, backend and agent status.
func (s *Store) GetRunProgress(ctx context.Context, id string) (*RunProgress, error) {
	var p RunProgress
	var f RunFacts
	err := s.Pool.QueryRow(ctx, `SELECT r.status, r.attempt, COALESCE(r.runner_kind, ''), `+runFacts("r")+`
		FROM runs r WHERE r.id = $1`, id).
		Scan(&p.Status, &p.Attempt, &p.RunnerKind, &f)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err == nil {
		p.AgentStatus = status.Derive(*f.run())
	}
	return &p, err
}

// Terminal reports whether a run status is final.
func Terminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusCancelled || status == StatusSkipped
}
