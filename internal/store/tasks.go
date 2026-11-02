package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Delegation (docs/ARCHITECTURE.md, Delegation): tasks an agent hands to
// other agents. Every transition is written here, in a transaction, guarded
// by the task's status; ReconcileTasks is the only writer that moves a task
// on its own.

// Task statuses. waiting: a dependency is not done. blocked: a person's
// decision is pending. working: a run in its conversation is queued or
// running. needs_input: its agent asked its parent something, or said it is
// blocked. done, failed, cancelled end it; a message reopens a done task.
const (
	TaskWaiting    = "waiting"
	TaskBlocked    = "blocked"
	TaskWorking    = "working"
	TaskNeedsInput = "needs_input"
	TaskDone       = "done"
	TaskFailed     = "failed"
	TaskCancelled  = "cancelled"
)

// A task run's outcome (its output's outcome field): it did the work, it
// could not (the task fails), or it cannot go on until something outside
// its reach changes (the task needs its parent's input). A run without the
// field succeeded.
const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeBlocked   = "blocked"
)

// Run triggers of delegation: a run in a task's conversation that starts
// the task or reads its parent's message, and a parent's wake when its
// tasks settle.
const (
	TriggerTask       = "task"
	TriggerDelegation = "delegation"
)

// DecisionType is the action a person resolves for a task that waits on
// their decision. orbit creates it, never an agent, and no rule approves it.
const DecisionType = "task:decision"

// TaskTerminal reports whether a task status ends it.
func TaskTerminal(status string) bool {
	return status == TaskDone || status == TaskFailed || status == TaskCancelled
}

// AgentAuthorPrefix marks a message one agent wrote to another: author is
// "agent:<name>". A person's username never has it (authentik's
// preferred_username has no colon).
const AgentAuthorPrefix = "agent:"

// Task is one delegated task as the API and a run's prompt show it.
type Task struct {
	ID                   string `json:"id"`
	Key                  string `json:"key"`
	Agent                string `json:"agent"`
	ConversationID       string `json:"conversation_id"`
	ParentAgent          string `json:"parent_agent"`
	ParentConversationID string `json:"parent_conversation_id"`
	ParentRunID          string `json:"parent_run_id"`
	ActionID             string `json:"action_id"`
	Brief                string `json:"brief"`
	Fresh                bool   `json:"fresh"`
	Depth                int    `json:"depth"`
	Status               string `json:"status"`
	// DependsOn is the keys of the tasks it waits for: of its batch, or
	// earlier tasks of its parent conversation.
	DependsOn []string      `json:"depends_on"`
	Decision  *TaskDecision `json:"decision"`
	Report    *TaskReport   `json:"report"`
	Error     *string       `json:"error"`
	// RunID and RunStatus are the newest run in its conversation.
	RunID     *string `json:"run_id"`
	RunStatus *string `json:"run_status"`
	// Outcome and OutcomeReason are what the newest run in its
	// conversation said of its work; nil until a run finishes with them.
	Outcome       *string `json:"outcome"`
	OutcomeReason *string `json:"outcome_reason"`
	// Attempt is its current attempt (1, and one more per task:retry);
	// Failures how many of its attempts failed by its agent's work.
	Attempt  int `json:"attempt"`
	Failures int `json:"failures"`
	// StalledAt is set while it is one of a stalled batch's open tasks the
	// parent was woken for (ReconcileTasks).
	StalledAt *time.Time `json:"stalled_at"`
	// Attempts are the attempts a retry ended, oldest first.
	Attempts []TaskAttempt `json:"attempts"`
	// Questions are its agent's open questions, to the parent.
	Questions  []ThreadQuestion `json:"questions"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
	FinishedAt *time.Time       `json:"finished_at"`
	// New is set for a parent run's prompt: the task finished or asked
	// since the parent's previous run started.
	New bool `json:"-"`
}

// OutcomeReasonText is OutcomeReason, or "" without one.
func (t *Task) OutcomeReasonText() string {
	if t.OutcomeReason == nil {
		return ""
	}
	return *t.OutcomeReason
}

// TaskAttempt is an earlier attempt of a task, as it was when a retry ended
// it.
type TaskAttempt struct {
	Attempt        int        `json:"attempt"`
	Agent          string     `json:"agent"`
	ConversationID string     `json:"conversation_id"`
	Brief          string     `json:"brief"`
	Error          *string    `json:"error"`
	FinishedAt     *time.Time `json:"finished_at"`
	RetryActionID  string     `json:"retry_action_id"`
}

// TaskDecision is a person's decision a task waits on.
type TaskDecision struct {
	ActionID string `json:"action_id"`
	Question string `json:"question"`
	// Status is the action's: pending until a person approves or rejects.
	Status    string  `json:"status"`
	DecidedBy *string `json:"decided_by"`
}

// TaskReport is the reply of the run that finished a task.
type TaskReport struct {
	RunID   string `json:"run_id"`
	Summary string `json:"summary"`
	Report  string `json:"report"`
}

// taskSelect reads tasks aliased t. newSince ($N, a run id or NULL) marks
// tasks that finished or asked after that run's conversation's previous
// run started.
const taskSelect = `
	SELECT t.id::text, t.key, t.agent, t.conversation_id::text, t.parent_agent, t.parent_conversation_id::text,
	       t.parent_run_id::text, t.action_id::text, t.brief, t.fresh, t.depth, t.status,
	       COALESCE((SELECT array_agg(d.key ORDER BY d.key) FROM task_deps x JOIN tasks d ON d.id = x.depends_on
	                 WHERE x.task_id = t.id), '{}'),
	       da.id::text, da.reason, da.status, da.decided_by,
	       t.report, t.error, lr.id, lr.status, lr.outcome, NULLIF(lr.outcome_reason, ''),
	       t.attempt, t.failures, t.stalled_at,
	       COALESCE((SELECT json_agg(json_build_object('attempt', a.attempt, 'agent', a.agent, 'conversation_id', a.conversation_id,
	                     'brief', a.brief, 'error', a.error, 'finished_at', a.finished_at, 'retry_action_id', a.retry_action_id) ORDER BY a.attempt)
	                 FROM task_attempts a WHERE a.task_id = t.id), '[]'),
	       COALESCE((SELECT json_agg(json_build_object('id', m.id, 'body', m.body, 'open', true) ORDER BY m.created_at, m.id)
	                 FROM messages m WHERE m.conversation_id = t.conversation_id AND ` + openQuestion + `), '[]'),
	       t.created_at, t.updated_at, t.finished_at
	FROM tasks t
	LEFT JOIN actions da ON da.id = t.decision_action_id
	LEFT JOIN LATERAL (SELECT r.id::text, r.status, r.output->>'outcome' AS outcome, r.output->>'outcome_reason' AS outcome_reason
	                   FROM runs r WHERE r.conversation_id = t.conversation_id
	                   ORDER BY r.created_at DESC LIMIT 1) lr ON true`

func scanTask(row pgx.Row) (*Task, error) {
	var t Task
	var decID, decQ, decStatus, decBy *string
	var report, attempts, questions []byte
	if err := row.Scan(&t.ID, &t.Key, &t.Agent, &t.ConversationID, &t.ParentAgent, &t.ParentConversationID,
		&t.ParentRunID, &t.ActionID, &t.Brief, &t.Fresh, &t.Depth, &t.Status, &t.DependsOn,
		&decID, &decQ, &decStatus, &decBy, &report, &t.Error, &t.RunID, &t.RunStatus, &t.Outcome, &t.OutcomeReason, &t.Attempt, &t.Failures, &t.StalledAt, &attempts, &questions,
		&t.CreatedAt, &t.UpdatedAt, &t.FinishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if decID != nil {
		t.Decision = &TaskDecision{ActionID: *decID, Question: *decQ, Status: *decStatus, DecidedBy: decBy}
	}
	if len(report) > 0 {
		t.Report = &TaskReport{}
		if err := json.Unmarshal(report, t.Report); err != nil {
			return nil, err
		}
	}
	if err := json.Unmarshal(attempts, &t.Attempts); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(questions, &t.Questions); err != nil {
		return nil, err
	}
	return &t, nil
}

func collectTasks(rows pgx.Rows, err error) ([]Task, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// GetTask returns one task.
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	return scanTask(s.Pool.QueryRow(ctx, taskSelect+` WHERE t.id = $1`, id))
}

// TaskForConversation returns the task whose own conversation this is, or
// nil when it is no task's.
func (s *Store) TaskForConversation(ctx context.Context, conversationID string) (*Task, error) {
	t, err := scanTask(s.Pool.QueryRow(ctx, taskSelect+` WHERE t.conversation_id = $1`, conversationID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return t, err
}

// ConversationTasks returns the tasks delegated from a conversation, oldest
// first. With runID, a task is New when it finished, or its agent asked,
// after the newest other run of that conversation started: what this run
// has not been told.
func (s *Store) ConversationTasks(ctx context.Context, conversationID, runID string) ([]Task, error) {
	rows, err := s.Pool.Query(ctx, taskSelect+` WHERE t.parent_conversation_id = $1 ORDER BY t.created_at, t.key`, conversationID)
	tasks, err := collectTasks(rows, err)
	if err != nil || runID == "" {
		return tasks, err
	}
	var prev *time.Time
	if err := s.Pool.QueryRow(ctx, `SELECT max(started_at) FROM runs
		WHERE conversation_id = $1 AND id <> $2 AND started_at IS NOT NULL`, conversationID, runID).Scan(&prev); err != nil {
		return nil, err
	}
	for i := range tasks {
		t := &tasks[i]
		t.New = prev == nil || t.UpdatedAt.After(*prev)
	}
	return tasks, nil
}

// ListTasksForRuns returns the tasks the given runs delegated, oldest first.
func (s *Store) ListTasksForRuns(ctx context.Context, runIDs []string) ([]Task, error) {
	rows, err := s.Pool.Query(ctx, taskSelect+` WHERE t.parent_run_id = ANY($1::uuid[]) ORDER BY t.created_at, t.key`, runIDs)
	return collectTasks(rows, err)
}

// Bounds on delegation. Each task is at least one full run of another
// agent, so a batch is a handful; nesting is shallow because each level
// multiplies the runs below it; and a conversation's total bounds a parent
// that keeps delegating from its own wakes.
const (
	MaxTasksPerDelegation = 8
	MaxTaskDepth          = 3
	MaxConversationTasks  = 64
	// MaxTaskFailures is the circuit breaker: a task whose attempts have
	// failed this many times reopens only when a person approves its
	// retry, so two agents cannot hand a hopeless task back and forth.
	MaxTaskFailures = 3
)

// NewTask is one task of a delegate action.
type NewTask struct {
	Key   string
	Agent string
	Brief string
	// After is the keys of the tasks it waits for: in the same batch, or
	// (a key the batch does not have) the newest earlier task of that key
	// the conversation delegated.
	After []string
	Fresh bool
	// Decision, when set, is a question a person must approve before the
	// task starts.
	Decision string
}

// Delegation is one delegate action being executed.
type Delegation struct {
	ActionID string
	// RunID is the run that proposed it: the parent.
	RunID string
	Tasks []NewTask
}

// Delegated is a task Delegate created: the handle a parent gets back.
type Delegated struct {
	ID             string  `json:"id"`
	Key            string  `json:"key"`
	Agent          string  `json:"agent"`
	ConversationID string  `json:"conversation_id"`
	DecisionID     *string `json:"decision_action_id,omitempty"`
}

// Delegate creates a batch of tasks in one transaction: for each, its own
// conversation of its agent with the brief as the parent's first message,
// its dependencies, and its decision (a pending task:decision action of the
// parent run). Nothing starts here; ReconcileTasks starts what is ready.
func (s *Store) Delegate(ctx context.Context, d Delegation) ([]Delegated, error) {
	if len(d.Tasks) == 0 || len(d.Tasks) > MaxTasksPerDelegation {
		return nil, fmt.Errorf("%w: a delegation has 1 to %d tasks", ErrInvalid, MaxTasksPerDelegation)
	}
	if err := checkDAG(d.Tasks); err != nil {
		return nil, err
	}
	var out []Delegated
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var parent, convo string
		var depth, existing int
		err := tx.QueryRow(ctx, `SELECT r.agent, r.conversation_id::text,
			       COALESCE((SELECT t.depth FROM tasks t WHERE t.conversation_id = r.conversation_id), 0),
			       (SELECT count(*) FROM tasks t WHERE t.parent_conversation_id = r.conversation_id)
			FROM runs r WHERE r.id = $1`, d.RunID).Scan(&parent, &convo, &depth, &existing)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if depth+1 > MaxTaskDepth {
			return fmt.Errorf("%w: delegation is nested at most %d deep, and this conversation is a task at depth %d",
				ErrInvalid, MaxTaskDepth, depth)
		}
		if existing+len(d.Tasks) > MaxConversationTasks {
			return fmt.Errorf("%w: a conversation delegates at most %d tasks, and this one has %d",
				ErrInvalid, MaxConversationTasks, existing)
		}
		ids := map[string]string{}
		for _, nt := range d.Tasks {
			var conv string
			if err := tx.QueryRow(ctx, `INSERT INTO conversations (agent, title) VALUES ($1, $2) RETURNING id::text`,
				nt.Agent, DefaultTitle(nt.Key+": "+nt.Brief)).Scan(&conv); err != nil {
				if isForeignKeyViolation(err) {
					return fmt.Errorf("%w: agent %q is not known", ErrInvalid, nt.Agent)
				}
				return err
			}
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO tasks (action_id, key, parent_run_id, parent_agent, parent_conversation_id,
				    agent, conversation_id, brief, fresh, depth, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, clock_timestamp()) RETURNING id::text`,
				d.ActionID, nt.Key, d.RunID, parent, convo, nt.Agent, conv, nt.Brief, nt.Fresh, depth+1).Scan(&id); err != nil {
				return err
			}
			// The brief is the parent's first message in the task's
			// conversation: the task's run reads it as its inbox, and the
			// thread shows who asked what.
			if _, err := tx.Exec(ctx, `INSERT INTO messages (agent, direction, body, author, conversation_id, created_at)
				VALUES ($1, 'to_agent', $2, $3, $4, clock_timestamp())`,
				nt.Agent, nt.Brief, AgentAuthorPrefix+parent, conv); err != nil {
				return err
			}
			ids[nt.Key] = id
			dl := Delegated{ID: id, Key: nt.Key, Agent: nt.Agent, ConversationID: conv}
			if q := strings.TrimSpace(nt.Decision); q != "" {
				params, _ := json.Marshal(map[string]string{"task_id": id, "key": nt.Key, "agent": nt.Agent, "question": q})
				var aid string
				if err := tx.QueryRow(ctx, `INSERT INTO actions (run_id, agent, type, params, reason, rule, status, created_at)
					VALUES ($1, $2, $3, $4, $5, 'approve', 'pending', clock_timestamp()) RETURNING id::text`,
					d.RunID, parent, DecisionType, params, q).Scan(&aid); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE tasks SET decision_action_id = $2, status = 'blocked' WHERE id = $1`, id, aid); err != nil {
					return err
				}
				if err := newAttention(ctx, tx, AttentionDecision, "action:"+aid, d.RunID, " needs a decision", &aid, nil, nil, nil); err != nil {
					return err
				}
				dl.DecisionID = &aid
			}
			out = append(out, dl)
		}
		for _, nt := range d.Tasks {
			for _, dep := range nt.After {
				on, ok := ids[dep]
				if !ok {
					// An earlier batch's task: it exists already and cannot
					// gain dependencies, so no cycle runs through it.
					err := tx.QueryRow(ctx, `SELECT id::text FROM tasks WHERE parent_conversation_id = $1 AND key = $2
						ORDER BY created_at DESC LIMIT 1`, convo, dep).Scan(&on)
					if errors.Is(err, pgx.ErrNoRows) {
						return fmt.Errorf("%w: task %q depends on %q, which is neither in this delegation nor a task this conversation delegated before",
							ErrInvalid, nt.Key, dep)
					}
					if err != nil {
						return err
					}
				}
				if _, err := tx.Exec(ctx, `INSERT INTO task_deps (task_id, depends_on) VALUES ($1, $2)`, ids[nt.Key], on); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if isUniqueViolation(err) {
		return nil, fmt.Errorf("%w: task keys must be unique within a delegation", ErrInvalid)
	}
	return out, err
}

// checkDAG holds a batch's dependencies to a DAG over its own keys. A key
// the batch does not have names an earlier task (Delegate resolves it),
// which no task of this batch can be upstream of.
func checkDAG(tasks []NewTask) error {
	byKey := map[string]*NewTask{}
	for i := range tasks {
		t := &tasks[i]
		if byKey[t.Key] != nil {
			return fmt.Errorf("%w: task key %q is used twice", ErrInvalid, t.Key)
		}
		byKey[t.Key] = t
	}
	for _, t := range tasks {
		for _, dep := range t.After {
			if dep == t.Key {
				return fmt.Errorf("%w: task %q depends on itself", ErrInvalid, t.Key)
			}
		}
	}
	// Kahn's algorithm: a cycle leaves tasks that never reach zero.
	indeg := map[string]int{}
	next := map[string][]string{}
	for _, t := range tasks {
		indeg[t.Key] += 0
		for _, dep := range t.After {
			if byKey[dep] == nil {
				continue
			}
			indeg[t.Key]++
			next[dep] = append(next[dep], t.Key)
		}
	}
	var ready []string
	for _, t := range tasks {
		if indeg[t.Key] == 0 {
			ready = append(ready, t.Key)
		}
	}
	seen := 0
	for len(ready) > 0 {
		k := ready[0]
		ready = ready[1:]
		seen++
		for _, n := range next[k] {
			if indeg[n]--; indeg[n] == 0 {
				ready = append(ready, n)
			}
		}
	}
	if seen != len(tasks) {
		return fmt.Errorf("%w: the tasks' dependencies form a cycle", ErrInvalid)
	}
	return nil
}

// ValidateDelegation checks a batch's shape (keys and dependencies) as
// Delegate will.
func ValidateDelegation(tasks []NewTask) error {
	if len(tasks) == 0 || len(tasks) > MaxTasksPerDelegation {
		return fmt.Errorf("%w: a delegation has 1 to %d tasks", ErrInvalid, MaxTasksPerDelegation)
	}
	return checkDAG(tasks)
}

// TaskMessage is a parent's message to one of its tasks.
type TaskMessage struct {
	// FromRun is the parent run whose action sends it; the task must be
	// one its conversation delegated.
	FromRun string
	TaskID  string
	Body    string
	// ReplyTo, when set, is the task's question it answers.
	ReplyTo string
}

// MessageTask writes a parent's message into a task's conversation and,
// unless a run there is already queued (it will read the message), queues
// one to read it. A done task reopens. It returns the message and the run
// that reads it.
func (s *Store) MessageTask(ctx context.Context, m TaskMessage) (msgID, runID string, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var agent, parent, conv, status string
		err := tx.QueryRow(ctx, `SELECT t.agent, t.parent_agent, t.conversation_id::text, t.status FROM tasks t
			JOIN runs r ON r.id = $2 AND r.conversation_id = t.parent_conversation_id
			WHERE t.id = $1 FOR UPDATE OF t`, m.TaskID, m.FromRun).Scan(&agent, &parent, &conv, &status)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("task %s is not one this conversation delegated: %w", m.TaskID, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if status == TaskFailed || status == TaskCancelled {
			return fmt.Errorf("task %s is %s: %w", m.TaskID, status, ErrConflict)
		}
		var replyTo any
		if m.ReplyTo != "" {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE id = $1 AND conversation_id = $2)`,
				m.ReplyTo, conv).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("%w: reply_to %s is not a message of task %s", ErrInvalid, m.ReplyTo, m.TaskID)
			}
			replyTo = m.ReplyTo
		}
		if err := tx.QueryRow(ctx, `INSERT INTO messages (agent, direction, body, author, reply_to, conversation_id, created_at)
			VALUES ($1, 'to_agent', $2, $3, $4, $5, clock_timestamp()) RETURNING id::text`,
			agent, m.Body, AgentAuthorPrefix+parent, replyTo, conv).Scan(&msgID); err != nil {
			return err
		}
		if status == TaskWaiting || status == TaskBlocked {
			// It has not started: its first run reads this with the brief.
			return nil
		}
		err = tx.QueryRow(ctx, `SELECT id::text FROM runs WHERE conversation_id = $1 AND status = 'queued' LIMIT 1`, conv).Scan(&runID)
		if errors.Is(err, pgx.ErrNoRows) {
			runID, err = enqueueTaskRun(ctx, tx, agent, conv, "")
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE tasks SET status = 'working', finished_at = NULL, report_due_at = NULL, updated_at = now()
			WHERE id = $1`, m.TaskID)
		return err
	})
	return msgID, runID, err
}

// TaskRetry is a parent's retry of one of its failed tasks.
type TaskRetry struct {
	// FromRun is the parent run whose action retries it; the task must be
	// one its conversation delegated.
	FromRun  string
	ActionID string
	TaskID   string
	Reason   string
	// Brief, when set, replaces the task's brief; Agent, when set and not
	// the task's, hands it to that agent in a new conversation.
	Brief string
	Agent string
	// ByPerson is set when a person approved the retry: the only way past
	// MaxTaskFailures.
	ByPerson bool
}

// Retried is what RetryTask did.
type Retried struct {
	Attempt        int    `json:"attempt"`
	Agent          string `json:"agent"`
	ConversationID string `json:"conversation_id"`
}

// RetryTask reopens a failed task on the same row: the attempt it ends is
// kept in task_attempts, the task waits again (ReconcileTasks starts it,
// with its dependencies' reports), and its dependents, which waited for
// it, stay as they are. Its conversation is told why it runs again; with a
// new agent, the task moves to a new conversation of that agent, whose
// first message is the brief. A task that failed MaxTaskFailures times
// reopens only ByPerson.
func (s *Store) RetryTask(ctx context.Context, r TaskRetry) (*Retried, error) {
	out := &Retried{}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var key, agent, parent, conv, status, brief string
		var errText *string
		var report []byte
		var finished *time.Time
		var attempt, failures int
		err := tx.QueryRow(ctx, `SELECT t.key, t.agent, t.parent_agent, t.conversation_id::text, t.status, t.brief, t.error, t.report,
			       t.finished_at, t.attempt, t.failures FROM tasks t
			JOIN runs r ON r.id = $2 AND r.conversation_id = t.parent_conversation_id
			WHERE t.id = $1 FOR UPDATE OF t`, r.TaskID, r.FromRun).
			Scan(&key, &agent, &parent, &conv, &status, &brief, &errText, &report, &finished, &attempt, &failures)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("task %s is not one this conversation delegated: %w", r.TaskID, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if status != TaskFailed {
			return fmt.Errorf("task %s is %s; only a failed task is retried: %w", r.TaskID, status, ErrConflict)
		}
		if failures >= MaxTaskFailures && !r.ByPerson {
			return fmt.Errorf("task %s failed %d times; only a person's approval reopens it: %w", r.TaskID, failures, ErrConflict)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO task_attempts (task_id, attempt, agent, conversation_id, brief, error, report, finished_at, retry_action_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, r.TaskID, attempt, agent, conv, brief, errText, report, finished, r.ActionID); err != nil {
			return err
		}
		newBrief := brief
		if b := strings.TrimSpace(r.Brief); b != "" {
			newBrief = b
		}
		note := fmt.Sprintf("Try this task again (attempt %d). The last attempt failed: %s\n\nWhy again: %s", attempt+1, deref(errText), r.Reason)
		if r.Agent != "" && r.Agent != agent {
			note = fmt.Sprintf("Another agent (%s) tried this task and failed: %s\n\nWhy you: %s", agent, deref(errText), r.Reason)
			agent = r.Agent
			if err := tx.QueryRow(ctx, `INSERT INTO conversations (agent, title) VALUES ($1, $2) RETURNING id::text`,
				agent, DefaultTitle(key+": "+newBrief)).Scan(&conv); err != nil {
				if isForeignKeyViolation(err) {
					return fmt.Errorf("%w: agent %q is not known", ErrInvalid, agent)
				}
				return err
			}
			if err := insertAgentMessage(ctx, tx, agent, parent, conv, newBrief); err != nil {
				return err
			}
		} else if newBrief != brief {
			note += "\n\nThe brief is now:\n\n" + newBrief
		}
		if err := insertAgentMessage(ctx, tx, agent, parent, conv, note); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET status = 'waiting', agent = $2, conversation_id = $3, brief = $4,
			    report = NULL, error = NULL, finished_at = NULL, report_due_at = NULL, attempt = attempt + 1, updated_at = now()
			WHERE id = $1`, r.TaskID, agent, conv, newBrief); err != nil {
			return err
		}
		*out = Retried{Attempt: attempt + 1, Agent: agent, ConversationID: conv}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// insertAgentMessage writes a message from agent parent into a task's
// conversation.
func insertAgentMessage(ctx context.Context, tx pgx.Tx, agent, parent, conv, body string) error {
	_, err := tx.Exec(ctx, `INSERT INTO messages (agent, direction, body, author, conversation_id, created_at)
		VALUES ($1, 'to_agent', $2, $3, $4, clock_timestamp())`, agent, body, AgentAuthorPrefix+parent, conv)
	return err
}

func enqueueTaskRun(ctx context.Context, tx pgx.Tx, agent, conv, prompt string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO runs (agent, spec_sha, trigger, prompt, conversation_id)
		SELECT name, spec_sha, 'task', $2, $3 FROM agents WHERE name = $1 RETURNING id::text`, agent, prompt, conv).Scan(&id)
	return id, err
}

// CancelTask ends a task that is not over: its queued runs are cancelled
// (a running one finishes, and its outcome is ignored), its pending
// decision rejected. With fromRun, only a task that run's conversation
// delegated; without, a person's. Its dependents are cancelled by the next
// reconcile.
func (s *Store) CancelTask(ctx context.Context, taskID, fromRun, reason, by string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var conv, action string
		var decision *string
		err := tx.QueryRow(ctx, `SELECT t.conversation_id::text, t.action_id::text, t.decision_action_id::text FROM tasks t
			WHERE t.id = $1 AND ($2::uuid IS NULL OR t.parent_conversation_id = (SELECT conversation_id FROM runs WHERE id = $2))
			FOR UPDATE`, taskID, nullString(fromRun)).Scan(&conv, &action, &decision)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("task %s: %w", taskID, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if reason == "" {
			reason = "cancelled"
		}
		tag, err := tx.Exec(ctx, `UPDATE tasks SET status = 'cancelled', error = $2, finished_at = now(), updated_at = now()
			WHERE id = $1 AND status NOT IN ('done', 'failed', 'cancelled')`, taskID, "cancelled by "+by+": "+reason)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("task %s is already over: %w", taskID, ErrConflict)
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET status = 'cancelled', finished_at = now()
			WHERE conversation_id = $1 AND status = 'queued'`, conv); err != nil {
			return err
		}
		if decision != nil {
			if _, err := tx.Exec(ctx, `UPDATE actions SET status = 'rejected', decided_by = $2, decided_at = now()
				WHERE id = $1 AND status = 'pending'`, *decision, by); err != nil {
				return err
			}
		}
		return markDueIfSettled(ctx, tx, action)
	})
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// markDueIfSettled marks a batch for its parent once every task in it is
// over.
func markDueIfSettled(ctx context.Context, tx pgx.Tx, actionID string) error {
	_, err := tx.Exec(ctx, `UPDATE tasks SET report_due_at = now()
		WHERE action_id = $1 AND status IN ('done', 'failed', 'cancelled') AND finished_at = (
		    SELECT max(finished_at) FROM tasks WHERE action_id = $1)
		  AND NOT EXISTS (SELECT 1 FROM tasks WHERE action_id = $1 AND status NOT IN ('done', 'failed', 'cancelled'))`, actionID)
	return err
}

// Reconciled is what one ReconcileTasks pass did.
type Reconciled struct {
	// Started is the runs it queued: tasks starting, parents waking.
	Started []string
	// Moved counts the tasks whose status changed.
	Moved int
}

// reconcileLock serializes reconciles across instances (an advisory lock
// key; any constant distinct from the scheduler's).
const reconcileLock = 0x6f72626974_7461 // "orbit" "ta"

// briefDepMax bounds one dependency's report in a dependent's first run;
// the whole report stays on the task.
const briefDepMax = 8 << 10

// ReconcileTasks moves every open task to where its rows say it is, and
// wakes parents whose tasks settled. It is idempotent and safe on any
// number of instances (one at a time holds the lock; the others return at
// once). Per task:
//
//   - waiting or blocked: a rejected decision or a cancelled dependency
//     cancels it; a pending decision blocks it; a failed dependency keeps
//     it waiting (its parent may retry that one); otherwise, once every
//     dependency is done, a run starts it, given their reports.
//   - working or needs_input: from its conversation. A queued or running
//     run, or an approved action not yet executed (a continuation about to
//     queue), is working; else the newest run decides: failed or cancelled
//     ends it so; succeeded with outcome failed fails it, keeping its
//     report; succeeded with an open question or outcome blocked is
//     needs_input; else done, with that run's report.
//
// A task needing input or failed is due to its parent at once, so it can
// answer, retry or reroute while the task's siblings run; any other
// finished one when its whole batch is over; a stalled batch's open tasks
// once per stall (markStalled). A parent conversation with a
// due task and no run since gets one delegation run, and never more than
// one waiting (runs_one_delegation).
func (s *Store) ReconcileTasks(ctx context.Context, decisionStall time.Duration) (*Reconciled, error) {
	res := &Reconciled{}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var got bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, int64(reconcileLock)).Scan(&got); err != nil || !got {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id::text, status, agent, conversation_id::text, action_id::text, decision_action_id::text
			FROM tasks WHERE status NOT IN ('done', 'failed', 'cancelled') ORDER BY created_at, key FOR UPDATE`)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, pgx.RowToAddrOfStructByPos[openTask])
		if err != nil {
			return err
		}
		settled := map[string]bool{}
		// A pass can free a task an earlier one in it waited on (a
		// dependency finished after its dependent was looked at), so passes
		// repeat until one moves nothing; each move is one-way, so at most
		// one pass per task is ever needed.
		for pass := 0; pass <= len(list); pass++ {
			moved := res.Moved
			if err := reconcilePass(ctx, tx, list, res, settled); err != nil {
				return err
			}
			if res.Moved == moved {
				break
			}
		}
		for action := range settled {
			if err := markDueIfSettled(ctx, tx, action); err != nil {
				return err
			}
		}
		if err := markStalled(ctx, tx, decisionStall); err != nil {
			return err
		}
		// A due task a parent run has started since is told; one with a
		// queued run in its parent conversation will be.
		if _, err := tx.Exec(ctx, `UPDATE tasks t SET report_due_at = NULL
			WHERE report_due_at IS NOT NULL AND EXISTS (SELECT 1 FROM runs r
			    WHERE r.conversation_id = t.parent_conversation_id AND r.started_at >= t.report_due_at)`); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `
			INSERT INTO runs (agent, spec_sha, trigger, conversation_id)
			SELECT DISTINCT a.name, a.spec_sha, 'delegation', t.parent_conversation_id
			FROM tasks t JOIN agents a ON a.name = t.parent_agent
			WHERE t.report_due_at IS NOT NULL
			  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.conversation_id = t.parent_conversation_id AND r.status = 'queued')
			ON CONFLICT (conversation_id) WHERE trigger = 'delegation' AND status = 'queued' DO NOTHING
			RETURNING id::text`)
		if err != nil {
			return err
		}
		woken, err := pgx.CollectRows(rows, pgx.RowTo[string])
		res.Started = append(res.Started, woken...)
		return err
	})
	return res, err
}

// markStalled finds batches none of whose open tasks can move: each waits,
// directly or through another open task, only on dependencies that failed,
// or on a person's decision pending longer than decisionStall. Nothing but
// the parent (a retry, a cancel) or a person will move them, so the
// parent is woken, once per stall: their stalled_at records it, and is
// cleared when a task can move again. A task working or needing input can
// move (needing input was due to the parent already).
func markStalled(ctx context.Context, tx pgx.Tx, decisionStall time.Duration) error {
	type open struct {
		ID, Action, Status string
		StaleDecision      bool
		Stalled            bool
	}
	rows, err := tx.Query(ctx, `SELECT t.id::text, t.action_id::text, t.status,
		       COALESCE(a.status = 'pending' AND a.created_at < now() - $1::interval, false), t.stalled_at IS NOT NULL
		FROM tasks t LEFT JOIN actions a ON a.id = t.decision_action_id
		WHERE t.status NOT IN ('done', 'failed', 'cancelled')`, decisionStall.String())
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[open])
	if err != nil || len(list) == 0 {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT x.task_id::text, x.depends_on::text FROM task_deps x
		JOIN tasks t ON t.id = x.task_id JOIN tasks d ON d.id = x.depends_on
		WHERE t.status = 'waiting' AND d.status NOT IN ('done', 'failed', 'cancelled')`)
	if err != nil {
		return err
	}
	type edge struct{ Task, Dep string }
	edges, err := pgx.CollectRows(rows, pgx.RowToStructByPos[edge])
	if err != nil {
		return err
	}
	byID := map[string]*open{}
	for i := range list {
		byID[list[i].ID] = &list[i]
	}
	waitsOn := map[string][]string{}
	for _, e := range edges {
		waitsOn[e.Task] = append(waitsOn[e.Task], e.Dep)
	}
	// Dependencies are a DAG, so the recursion ends.
	memo := map[string]bool{}
	var movable func(id string) bool
	movable = func(id string) bool {
		if m, ok := memo[id]; ok {
			return m
		}
		t := byID[id]
		m := false
		switch t.Status {
		case TaskWorking, TaskNeedsInput:
			m = true
		case TaskBlocked:
			m = !t.StaleDecision
		case TaskWaiting:
			for _, d := range waitsOn[id] {
				if byID[d] != nil && movable(d) {
					m = true
					break
				}
			}
		}
		memo[id] = m
		return m
	}
	stuck := map[string]bool{}
	for _, t := range list {
		stuck[t.Action] = true
	}
	for _, t := range list {
		if movable(t.ID) {
			stuck[t.Action] = false
		}
	}
	var wake, clear []string
	for _, t := range list {
		switch {
		case stuck[t.Action] && !t.Stalled:
			wake = append(wake, t.ID)
		case !stuck[t.Action] && t.Stalled:
			clear = append(clear, t.ID)
		}
	}
	if len(clear) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET stalled_at = NULL WHERE id = ANY($1::uuid[])`, clear); err != nil {
			return err
		}
	}
	if len(wake) > 0 {
		// The whole batch is marked, so a later reconcile does not wake for
		// the tasks already reported.
		if _, err := tx.Exec(ctx, `UPDATE tasks SET stalled_at = now(), report_due_at = now(), updated_at = now()
			WHERE action_id IN (SELECT action_id FROM tasks WHERE id = ANY($1::uuid[]))
			  AND status NOT IN ('done', 'failed', 'cancelled')`, wake); err != nil {
			return err
		}
	}
	return nil
}

// openTask is a task ReconcileTasks holds locked.
type openTask struct {
	ID, Status, Agent, Conv, Action string
	Decision                        *string
}

// reconcilePass moves each task in list once, recording its new status in
// list, the runs it queued and the batches it finished in res and settled.
func reconcilePass(ctx context.Context, tx pgx.Tx, list []*openTask, res *Reconciled, settled map[string]bool) error {
	for _, t := range list {
		if TaskTerminal(t.Status) {
			continue
		}
		m, err := reconcileOne(ctx, tx, t.ID, t.Status, t.Conv, t.Decision)
		if err != nil {
			return fmt.Errorf("task %s: %w", t.ID, err)
		}
		next, report, errText, prompt := m.next, m.report, m.errText, m.prompt
		if next == t.Status && prompt == "" {
			continue
		}
		if next == TaskWorking && (t.Status == TaskWaiting || t.Status == TaskBlocked) {
			var active bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE conversation_id = $1 AND status IN ('queued', 'running'))`,
				t.Conv).Scan(&active); err != nil {
				return err
			}
			if !active {
				id, err := enqueueTaskRun(ctx, tx, t.Agent, t.Conv, prompt)
				if err != nil {
					return err
				}
				res.Started = append(res.Started, id)
			}
		}
		if next == t.Status {
			continue
		}
		t.Status = next
		res.Moved++
		var rep any
		if report != nil {
			b, _ := json.Marshal(report)
			rep = b
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET status = $2, report = COALESCE($3, report), error = NULLIF($4, ''),
			    failures = failures + $5::int, updated_at = now(),
			    finished_at = CASE WHEN $2 IN ('done', 'failed', 'cancelled') THEN now() END,
			    report_due_at = CASE WHEN $2 IN ('needs_input', 'failed') THEN now() ELSE report_due_at END
			WHERE id = $1`, t.ID, next, rep, errText, m.failure); err != nil {
			return err
		}
		if TaskTerminal(next) {
			settled[t.Action] = true
		}
	}
	return nil
}

// move is where reconcileOne puts a task: its next status, the report and
// error it settles with, the first run's prompt for one that starts, and
// failure 1 when it fails by its agent's work (the circuit breaker's
// count; a run lost with its worker is not that).
type move struct {
	next    string
	report  *TaskReport
	errText string
	prompt  string
	failure int
}

// reconcileOne decides where one open task is. For a task that starts now
// it returns working and the first run's prompt: its dependencies' reports
// and the decision that let it go.
func reconcileOne(ctx context.Context, tx pgx.Tx, id, status, conv string, decision *string) (move, error) {
	if status == TaskWaiting || status == TaskBlocked {
		var b strings.Builder
		if decision != nil {
			var st string
			var by *string
			var q string
			if err := tx.QueryRow(ctx, `SELECT status, decided_by, reason FROM actions WHERE id = $1`, *decision).Scan(&st, &by, &q); err != nil {
				return move{}, err
			}
			switch st {
			case "pending":
				return move{next: TaskBlocked}, nil
			case "approved", "executing", "executed":
				fmt.Fprintf(&b, "A person (%s) approved this task to go ahead: %q\n\n", deref(by), q)
			default:
				return move{next: TaskCancelled, errText: fmt.Sprintf("a person (%s) declined: %s", deref(by), q)}, nil
			}
		}
		type dep struct {
			Key, Agent, Status string
			Report             []byte
		}
		rows, err := tx.Query(ctx, `SELECT d.key, d.agent, d.status, d.report FROM task_deps x JOIN tasks d ON d.id = x.depends_on
			WHERE x.task_id = $1 ORDER BY d.key`, id)
		if err != nil {
			return move{}, err
		}
		deps, err := pgx.CollectRows(rows, pgx.RowToStructByPos[dep])
		if err != nil {
			return move{}, err
		}
		for _, d := range deps {
			if d.Status == TaskCancelled {
				return move{next: TaskCancelled, errText: fmt.Sprintf("dependency %q cancelled", d.Key)}, nil
			}
		}
		for _, d := range deps {
			// A failed dependency holds it: its parent may retry that one.
			if d.Status != TaskDone {
				return move{next: TaskWaiting}, nil
			}
		}
		if len(deps) > 0 {
			b.WriteString("This task waited for these tasks, now done. Their reports:\n")
			for _, d := range deps {
				var r TaskReport
				_ = json.Unmarshal(d.Report, &r)
				fmt.Fprintf(&b, "\n## %s (%s): %s\n\n%s\n", d.Key, d.Agent, r.Summary, clipText(strings.TrimSpace(r.Report), briefDepMax))
			}
		}
		p := strings.TrimSpace(b.String())
		if p == "" {
			p = "Start the task."
		}
		return move{next: TaskWorking, prompt: p}, nil
	}
	var active, pendingExec, asked bool
	if err := tx.QueryRow(ctx, `SELECT
		    EXISTS (SELECT 1 FROM runs WHERE conversation_id = $1 AND status IN ('queued', 'running')),
		    EXISTS (SELECT 1 FROM actions a JOIN runs r ON r.id = a.run_id
		            WHERE r.conversation_id = $1 AND a.status IN ('approved', 'executing')),
		    EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = $1 AND `+openQuestion+`)`, conv).
		Scan(&active, &pendingExec, &asked); err != nil {
		return move{}, err
	}
	if active || pendingExec {
		return move{next: TaskWorking}, nil
	}
	var runID, st string
	var summary, rep, rerr, outcome, why *string
	var lost bool
	err := tx.QueryRow(ctx, `SELECT r.id::text, r.status, r.summary, r.report, r.error, r.output->>'outcome', r.output->>'outcome_reason',
		       COALESCE((SELECT a.outcome = 'lost' FROM run_attempts a WHERE a.run_id = r.id ORDER BY a.attempt DESC LIMIT 1), false)
		FROM runs r WHERE r.conversation_id = $1
		ORDER BY r.created_at DESC LIMIT 1`, conv).Scan(&runID, &st, &summary, &rep, &rerr, &outcome, &why, &lost)
	if errors.Is(err, pgx.ErrNoRows) {
		// Started with no run (its first one was cancelled by a person):
		// nothing will move it, so it is over.
		return move{next: TaskCancelled, errText: "no run is working on it"}, nil
	}
	if err != nil {
		return move{}, err
	}
	switch st {
	case StatusFailed:
		// A run the reaper failed because its worker was lost says nothing
		// about the task's work: it does not count toward the breaker.
		m := move{next: TaskFailed, errText: "run failed: " + deref(rerr), failure: 1}
		if lost {
			m.failure = 0
		}
		return m, nil
	case StatusCancelled:
		return move{next: TaskCancelled, errText: "its run was cancelled"}, nil
	}
	report := &TaskReport{RunID: runID, Summary: deref(summary), Report: deref(rep)}
	reason := strings.TrimSpace(deref(why))
	if reason == "" {
		reason = report.Summary
	}
	switch {
	case deref(outcome) == OutcomeFailed:
		// The run finished, but says it could not do the work: the task
		// failed, with what the run reported kept for its parent.
		return move{next: TaskFailed, report: report, errText: "the agent could not do the work: " + reason, failure: 1}, nil
	case asked, deref(outcome) == OutcomeBlocked:
		return move{next: TaskNeedsInput}, nil
	}
	return move{next: TaskDone, report: report}, nil
}

// clipText cuts s to at most n bytes on a rune boundary and says so.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n\n[clipped: the full report is on the task]"
}
