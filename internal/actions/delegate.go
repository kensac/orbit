package actions

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/kensac/orbit/internal/forms"
	"github.com/kensac/orbit/internal/store"
	templatelib "github.com/kensac/orbit/internal/templates"
)

// Delegation's action types (docs/ARCHITECTURE.md, Delegation). An agent
// hands tasks to the agents its spec's delegates name; it hears back when
// they settle, as a delegation run, and manages them by id.
const (
	DelegateType    = "delegate"
	TaskMessageType = "task:message"
	TaskCancelType  = "task:cancel"
	TaskRetryType   = "task:retry"
)

// IsTaskType reports whether an action type is one of delegation's: the
// ones a person's own conversation applies directly.
func IsTaskType(t string) bool {
	return t == DelegateType || t == TaskMessageType || t == TaskCancelType || t == TaskRetryType
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
	Key        string                     `json:"key"`
	Agent      string                     `json:"agent,omitempty"`
	Template   string                     `json:"template,omitempty"`
	Parameters map[string]json.RawMessage `json:"parameters,omitempty"`
	Connectors []string                   `json:"connectors,omitempty"`
	Brief      string                     `json:"brief"`
	After      []string                   `json:"after,omitempty"`
	Fresh      bool                       `json:"fresh,omitempty"`
	Decision   string                     `json:"decision,omitempty"`
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

// TaskRetryParams is a task:retry action's params: the failed task, why it
// is worth another attempt, and optionally a new brief or another agent.
type TaskRetryParams struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason"`
	Brief  string `json:"brief,omitempty"`
	Agent  string `json:"agent,omitempty"`
}

// TaskOps is what delegation's actions do, given the action that asks.
type TaskOps interface {
	Delegate(ctx context.Context, c Call, p DelegateParams) (json.RawMessage, error)
	MessageTask(ctx context.Context, c Call, p TaskMessageParams) (json.RawMessage, error)
	CancelTask(ctx context.Context, c Call, p TaskCancelParams) (json.RawMessage, error)
	RetryTask(ctx context.Context, c Call, p TaskRetryParams) (json.RawMessage, error)
}

var uuidSchema = map[string]any{"type": "string", "pattern": "^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$"}

func delegateType(targets []string, templates []store.Template, connectors []string) Type {
	taskSchemas := []any{agentTaskSchema(targets)}
	var templateNames []string
	for _, tpl := range templates {
		if hasUserPrompt(tpl) {
			continue
		}
		templateNames = append(templateNames, tpl.Name)
		var specConnectors struct {
			Connectors []string `json:"connectors"`
		}
		_ = json.Unmarshal(tpl.Spec, &specConnectors)
		allowed := make([]string, 0, len(connectors))
		for _, name := range connectors {
			if slices.Contains(specConnectors.Connectors, name) {
				allowed = append(allowed, name)
			}
		}
		taskSchemas = append(taskSchemas, templateTaskSchema(tpl, allowed))
	}
	return delegateTypeWithSchemas(targets, templateNames, taskSchemas, templates, connectors)
}

func delegateTypeWithSchemas(targets, templateNames []string, taskSchemas []any, templates []store.Template, connectors []string) Type {
	destinations := append([]string(nil), targets...)
	for _, name := range templateNames {
		destinations = append(destinations, "template:"+name)
	}
	return Type{
		Name: DelegateType,
		Description: fmt.Sprintf("Hand tasks to other agents or template workers (%s). A template task gives typed parameters for that template; only templates without person-only parameters are offered. Template workers are fresh and use only connectors you select from both your own and the template's connector sets. Each task runs in its own conversation and reports back to you; tasks of different agents run in parallel, an agent's own tasks one at a time. "+
			"after lists keys of tasks that must be done first, in this same action or (a key it does not have) the newest earlier task of that key you delegated from this conversation; a dependent task is given their reports. "+
			"fresh starts the agent without its memory (a worker from its template). "+
			"decision is a question a person must approve before the task starts. "+
			"You are woken with every report once all tasks are over, and as soon as one fails or asks you something; do not wait or poll. "+
			"At most %d tasks per action.", strings.Join(destinations, ", "), store.MaxTasksPerDelegation),
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"tasks": map[string]any{
					"type": "array", "minItems": 1, "maxItems": store.MaxTasksPerDelegation,
					"items": map[string]any{"type": "object", "oneOf": taskSchemas},
				},
			},
			"required":             []string{"tasks"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			decoded, err := DecodeDelegate(p, targets, templateNames)
			if err != nil {
				return err
			}
			byName := make(map[string]store.Template, len(templates))
			for _, tpl := range templates {
				byName[tpl.Name] = tpl
			}
			for _, task := range decoded.Tasks {
				if task.Template == "" {
					continue
				}
				tpl := byName[task.Template]
				form := templatelib.Template{Parameters: tpl.Parameters}
				if _, err := forms.Answers(form.Fields(), task.Parameters); err != nil {
					return fmt.Errorf("delegate: task %q: %w", task.Key, err)
				}
				for _, name := range task.Connectors {
					if !slices.Contains(connectors, name) {
						return fmt.Errorf("delegate: task %q: connector %q is not enabled for this agent", task.Key, name)
					}
				}
			}
			return nil
		},
	}
}

func baseTaskProperties() map[string]any {
	return map[string]any{
		"key":      map[string]any{"type": "string", "pattern": taskKeyRE.String()},
		"brief":    map[string]any{"type": "string", "minLength": 1, "maxLength": briefMax},
		"after":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		"fresh":    map[string]any{"type": "boolean"},
		"decision": map[string]any{"type": "string", "maxLength": 2000},
	}
}

func agentTaskSchema(targets []string) map[string]any {
	props := baseTaskProperties()
	props["agent"] = map[string]any{"type": "string", "enum": targets}
	return map[string]any{"type": "object", "properties": props, "required": []string{"key", "agent", "brief"}, "additionalProperties": false}
}

func templateTaskSchema(t store.Template, connectors []string) map[string]any {
	props := baseTaskProperties()
	props["template"] = map[string]any{"const": t.Name}
	props["parameters"] = templateParametersSchema(t.Parameters)
	props["connectors"] = map[string]any{"type": "array", "uniqueItems": true, "items": map[string]any{"type": "string", "enum": connectors}}
	return map[string]any{"type": "object", "properties": props, "required": []string{"key", "template", "brief"}, "additionalProperties": false}
}

func templateParametersSchema(params []templatelib.Parameter) map[string]any {
	props := map[string]any{}
	var required []string
	for _, p := range params {
		if p.Mode == templatelib.UserPrompt {
			continue
		}
		field := map[string]any{}
		switch p.Type {
		case forms.Number:
			field["type"] = "number"
		case forms.Boolean:
			field["type"] = "boolean"
		default:
			field["type"] = "string"
			if p.Type == forms.Select {
				field["enum"] = p.Options
			}
			if p.Type == forms.Date {
				field["format"] = "date"
			}
		}
		if p.Mode == templatelib.Required && !p.Field().HasDefault() {
			required = append(required, p.Name)
		}
		props[p.Name] = field
	}
	out := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func hasUserPrompt(t store.Template) bool {
	for _, p := range t.Parameters {
		if p.Mode == templatelib.UserPrompt {
			return true
		}
	}
	return false
}

// DecodeDelegate decodes and checks a delegate action's params: every
// agent one of targets, keys well formed, dependencies a DAG of this
// action's own keys.
func DecodeDelegate(p json.RawMessage, targets []string, templateTargets ...[]string) (DelegateParams, error) {
	var v DelegateParams
	if err := strictDecode(p, &v); err != nil {
		return v, err
	}
	var templates []string
	if len(templateTargets) > 0 {
		templates = templateTargets[0]
	}
	var batch []store.NewTask
	for i, t := range v.Tasks {
		if !taskKeyRE.MatchString(t.Key) {
			return v, fmt.Errorf("delegate: task %d: key %q must match %s", i, t.Key, taskKeyRE)
		}
		if (t.Agent == "") == (t.Template == "") {
			return v, fmt.Errorf("delegate: task %q: give exactly one of agent or template", t.Key)
		}
		if t.Agent != "" {
			if !slices.Contains(targets, t.Agent) {
				return v, fmt.Errorf("delegate: task %q: agent %q is not one this agent may delegate to", t.Key, t.Agent)
			}
			if len(t.Parameters) > 0 || len(t.Connectors) > 0 {
				return v, fmt.Errorf("delegate: task %q: parameters and connectors are only for template workers", t.Key)
			}
		} else if !slices.Contains(templates, t.Template) {
			return v, fmt.Errorf("delegate: task %q: template %q is not one this agent may delegate to", t.Key, t.Template)
		}
		connectorNames := slices.Clone(t.Connectors)
		sort.Strings(connectorNames)
		if len(t.Connectors) != len(slices.Compact(connectorNames)) {
			return v, fmt.Errorf("delegate: task %q: connectors must be unique", t.Key)
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
	return store.NewTask{Key: t.Key, Agent: t.Agent, TemplateName: t.Template, Parameters: t.Parameters, Connectors: t.Connectors,
		Brief: strings.TrimSpace(t.Brief), After: t.After, Fresh: t.Fresh || t.Template != "", Decision: strings.TrimSpace(t.Decision)}
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
		Description: "Write to one of your delegated tasks by its task_id: answer its question (reply_to is the question's id), correct it, or add to its brief. " +
			"While its agent is working on it in a chat turn, the message goes into that turn and the result says delivered: steered (it reads it at its next tool call); " +
			"otherwise delivered: queued, and its agent is woken to read it (a done task reopens). You hear back as for delegate.",
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

func taskRetryType(targets []string) Type {
	return Type{
		Name: TaskRetryType,
		Description: fmt.Sprintf("Try one of your failed tasks again by its task_id, on the same task: its dependents still wait for it. "+
			"brief replaces its brief; agent (one of %s) hands it to another agent, in a new conversation. reason says what changes this time. "+
			"After %d failed attempts a task reopens only when a person approves the retry.", strings.Join(targets, ", "), store.MaxTaskFailures),
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": uuidSchema,
				"reason":  map[string]any{"type": "string", "minLength": 1, "maxLength": 2000},
				"brief":   map[string]any{"type": "string", "minLength": 1, "maxLength": briefMax},
				"agent":   map[string]any{"type": "string", "enum": targets},
			},
			"required":             []string{"task_id", "reason"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			_, err := DecodeTaskRetry(p, targets)
			return err
		},
	}
}

// DecodeTaskRetry decodes and checks a task:retry action's params: a new
// agent, when named, one of targets.
func DecodeTaskRetry(p json.RawMessage, targets []string) (TaskRetryParams, error) {
	var v TaskRetryParams
	if err := strictDecode(p, &v); err != nil {
		return v, err
	}
	v.Reason, v.Brief = strings.TrimSpace(v.Reason), strings.TrimSpace(v.Brief)
	if v.TaskID == "" || v.Reason == "" {
		return v, fmt.Errorf("task:retry: task_id and reason are required")
	}
	if len(v.Brief) > briefMax {
		return v, fmt.Errorf("task:retry: brief must be at most %d characters", briefMax)
	}
	if v.Agent != "" && !slices.Contains(targets, v.Agent) {
		return v, fmt.Errorf("task:retry: agent %q is not one this agent may delegate to", v.Agent)
	}
	return v, nil
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
	case TaskRetryType:
		var v TaskRetryParams
		if err := strictDecode(c.Params, &v); err != nil {
			return nil, err
		}
		return e.Tasks.RetryTask(ctx, c, v)
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
