package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kensac/orbit/internal/store"
)

// Delegation's action types (docs/ARCHITECTURE.md, Delegation). An agent
// hands tasks to the agents its spec's delegates name; it hears back when
// they settle, as a delegation run, and manages them by id.
const (
	DelegateType    = "delegate"
	TaskMessageType = "task:message"
	TaskCancelType  = "task:cancel"
)

// IsTaskType reports whether an action type is one of delegation's: the
// ones a person's own conversation applies directly.
func IsTaskType(t string) bool {
	return t == DelegateType || t == TaskMessageType || t == TaskCancelType
}

// briefMax bounds a task's brief: what one agent tells another, which
// becomes the first message of the task's conversation.
const briefMax = 20000

// taskKeyRE is a task key: short, so a dependency list and a prompt's
// table stay readable.
var taskKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// DelegateParams is a delegate action's params.
type DelegateParams struct {
	Tasks []DelegateTask `json:"tasks"`
}

// DelegateTask is one task of a delegate action.
type DelegateTask struct {
	Key      string   `json:"key"`
	Agent    string   `json:"agent"`
	Brief    string   `json:"brief"`
	After    []string `json:"after,omitempty"`
	Fresh    bool     `json:"fresh,omitempty"`
	Decision string   `json:"decision,omitempty"`
}

// TaskMessageParams is a task:message action's params.
type TaskMessageParams struct {
	TaskID  string `json:"task_id"`
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to,omitempty"`
}

// TaskCancelParams is a task:cancel action's params.
type TaskCancelParams struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
}

// TaskOps is what delegation's actions do, given the action that asks.
type TaskOps interface {
	Delegate(ctx context.Context, c Call, p DelegateParams) (json.RawMessage, error)
	MessageTask(ctx context.Context, c Call, p TaskMessageParams) (json.RawMessage, error)
	CancelTask(ctx context.Context, c Call, p TaskCancelParams) (json.RawMessage, error)
}

var uuidSchema = map[string]any{"type": "string", "pattern": "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"}

func delegateType(targets []string) Type {
	return Type{
		Name: DelegateType,
		Description: fmt.Sprintf("Hand tasks to other agents (%s). Each task runs in its own conversation of its agent, "+
			"which gets the brief and reports back to you; tasks of different agents run in parallel, an agent's own tasks one at a time. "+
			"after lists keys of tasks in this same action that must be done first; a dependent task is given their reports. "+
			"fresh starts the agent without its memory (a worker from its template). "+
			"decision is a question a person must approve before the task starts. "+
			"You are woken with every report once all tasks are over, or as soon as one asks you something; do not wait or poll. "+
			"At most %d tasks per action.", strings.Join(targets, ", "), store.MaxTasksPerDelegation),
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tasks": map[string]any{
					"type": "array", "minItems": 1, "maxItems": store.MaxTasksPerDelegation,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"key":      map[string]any{"type": "string", "pattern": taskKeyRE.String()},
							"agent":    map[string]any{"type": "string", "enum": targets},
							"brief":    map[string]any{"type": "string", "minLength": 1, "maxLength": briefMax},
							"after":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"fresh":    map[string]any{"type": "boolean"},
							"decision": map[string]any{"type": "string", "maxLength": 2000},
						},
						"required":             []string{"key", "agent", "brief"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"tasks"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			_, err := DecodeDelegate(p, targets)
			return err
		},
	}
}

// DecodeDelegate decodes and checks a delegate action's params: every
// agent one of targets, keys well formed, dependencies a DAG of this
// action's own keys.
func DecodeDelegate(p json.RawMessage, targets []string) (DelegateParams, error) {
	var v DelegateParams
	if err := strictDecode(p, &v); err != nil {
		return v, err
	}
	var batch []store.NewTask
	for i, t := range v.Tasks {
		if !taskKeyRE.MatchString(t.Key) {
			return v, fmt.Errorf("delegate: task %d: key %q must match %s", i, t.Key, taskKeyRE)
		}
		if !slices.Contains(targets, t.Agent) {
			return v, fmt.Errorf("delegate: task %q: agent %q is not one this agent may delegate to", t.Key, t.Agent)
		}
		if strings.TrimSpace(t.Brief) == "" || len(t.Brief) > briefMax {
			return v, fmt.Errorf("delegate: task %q: brief must be 1 to %d characters", t.Key, briefMax)
		}
		batch = append(batch, toNewTask(t))
	}
	if err := store.ValidateDelegation(batch); err != nil {
		return v, fmt.Errorf("delegate: %w", err)
	}
	return v, nil
}

func toNewTask(t DelegateTask) store.NewTask {
	return store.NewTask{Key: t.Key, Agent: t.Agent, Brief: strings.TrimSpace(t.Brief), After: t.After, Fresh: t.Fresh,
		Decision: strings.TrimSpace(t.Decision)}
}

// NewTasks is a decoded delegate action's tasks as the store takes them.
func (p DelegateParams) NewTasks() []store.NewTask {
	out := make([]store.NewTask, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		out = append(out, toNewTask(t))
	}
	return out
}

func taskMessageType() Type {
	return Type{
		Name: TaskMessageType,
		Description: "Write to one of your delegated tasks by its task_id: answer its question (reply_to is the question's id) or add to its brief. " +
			"Its agent is woken to read it; a done task reopens. You hear back as for delegate.",
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id":  uuidSchema,
				"body":     map[string]any{"type": "string", "minLength": 1, "maxLength": briefMax},
				"reply_to": uuidSchema,
			},
			"required":             []string{"task_id", "body"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			var v TaskMessageParams
			if err := strictDecode(p, &v); err != nil {
				return err
			}
			if strings.TrimSpace(v.Body) == "" || v.TaskID == "" {
				return fmt.Errorf("task:message: task_id and body are required")
			}
			return nil
		},
	}
}

func taskCancelType() Type {
	return Type{
		Name:        TaskCancelType,
		Description: "Cancel one of your delegated tasks by its task_id; the tasks that depend on it are cancelled too.",
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": uuidSchema,
				"reason":  map[string]any{"type": "string", "minLength": 1, "maxLength": 2000},
			},
			"required":             []string{"task_id", "reason"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			var v TaskCancelParams
			if err := strictDecode(p, &v); err != nil {
				return err
			}
			if v.TaskID == "" {
				return fmt.Errorf("task:cancel: task_id is required")
			}
			return nil
		},
	}
}

// executeTask decodes a delegation action's params again (an action is
// executed from its stored row) and hands it to Tasks.
func (e *Executor) executeTask(ctx context.Context, c Call) (json.RawMessage, error) {
	switch c.Type {
	case DelegateType:
		var v DelegateParams
		if err := strictDecode(c.Params, &v); err != nil {
			return nil, err
		}
		return e.Tasks.Delegate(ctx, c, v)
	case TaskMessageType:
		var v TaskMessageParams
		if err := strictDecode(c.Params, &v); err != nil {
			return nil, err
		}
		return e.Tasks.MessageTask(ctx, c, v)
	default:
		var v TaskCancelParams
		if err := strictDecode(c.Params, &v); err != nil {
			return nil, err
		}
		return e.Tasks.CancelTask(ctx, c, v)
	}
}
