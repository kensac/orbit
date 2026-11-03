package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/forms"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/templates"
)

func (e *Engine) actionCatalog(ctx context.Context, a *spec.Agent, d spec.Deployment) ([]actions.Type, error) {
	if len(a.DelegateTemplateTargets()) == 0 || a.RuleFor(actions.DelegateType) == spec.RuleDeny {
		return actions.Catalog(a, d), nil
	}
	templates, err := e.Store.ListTemplates(ctx, "active")
	if err != nil {
		return nil, err
	}
	full := make([]store.Template, 0, len(templates))
	for _, tpl := range templates {
		if !slices.Contains(a.DelegateTemplateTargets(), "*") && !slices.Contains(a.DelegateTemplateTargets(), tpl.Name) {
			continue
		}
		loaded, err := e.Store.GetTemplate(ctx, tpl.Name, tpl.Version, true)
		if err != nil {
			return nil, err
		}
		if loaded.Status == "active" {
			full = append(full, *loaded)
		}
	}
	return actions.Catalog(a, d, full), nil
}

// Delegation (docs/ARCHITECTURE.md, Delegation): the engine's side is the
// prompts (a task's run is told whom it reports to; a parent's run is told
// where its tasks are), the actions' executor, and the reconcile that moves
// tasks and wakes parents.

// ReconcileTasks moves every open task and wakes every parent due one
// (store.ReconcileTasks), and nudges the executors when it queued runs.
// Control calls it after each run, after delegation actions, and on every
// outbox tick, so a decision a person resolves is picked up within one.
func (e *Engine) ReconcileTasks(ctx context.Context) error {
	res, err := e.Store.ReconcileTasks(ctx, time.Duration(e.Current().Config.Deployment.Defaults.DecisionStall))
	if err != nil {
		return err
	}
	if len(res.Started) > 0 {
		e.Log.Info("delegation: queued runs", "runs", res.Started, "moved", res.Moved)
		if e.Wake != nil {
			e.Wake()
		}
	}
	return nil
}

// taskOps carries out delegation's actions for the action loop.
type taskOps struct {
	e    *Engine
	snap *Snapshot
}

func (o *taskOps) Delegate(ctx context.Context, c actions.Call, p actions.DelegateParams) (json.RawMessage, error) {
	a, ok := o.snap.Config.Agents[c.Agent]
	if !ok {
		return nil, fmt.Errorf("delegate: agent %q is no longer configured", c.Agent)
	}
	// Checked again against the configuration in force: a target may have
	// gone since the run proposed it.
	available, err := o.e.Store.ListTemplates(ctx, "active")
	if err != nil {
		return nil, err
	}
	targetTemplates := make([]string, 0, len(available))
	for _, tpl := range available {
		if slices.Contains(a.DelegateTemplateTargets(), "*") || slices.Contains(a.DelegateTemplateTargets(), tpl.Name) {
			targetTemplates = append(targetTemplates, tpl.Name)
		}
	}
	if _, err := actions.DecodeDelegate(c.Params, a.DelegateTargets(), targetTemplates); err != nil {
		return nil, err
	}
	tasks := p.NewTasks()
	for i := range tasks {
		if tasks[i].TemplateName == "" {
			continue
		}
		if err := o.prepareTemplateWorker(ctx, c, a, &tasks[i]); err != nil {
			return nil, fmt.Errorf("delegate task %q: %w", tasks[i].Key, err)
		}
	}
	made, err := o.e.Store.Delegate(ctx, store.Delegation{ActionID: c.ID, RunID: c.RunID, Tasks: tasks})
	if err != nil {
		return nil, err
	}
	parent, err := o.e.Store.GetRun(ctx, c.RunID)
	if err != nil {
		return nil, err
	}
	for i, d := range made {
		if d.DecisionID == nil || Attended(parent.Trigger) {
			// A person in the conversation sees the decision there.
			continue
		}
		o.e.NotifyApproval(ctx, c.Agent, c.RunID, *d.DecisionID,
			fmt.Sprintf("%s: may task %q (%s) go ahead?", c.Agent, d.Key, d.Agent), p.Tasks[i].Decision)
	}
	if err := o.e.ReconcileTasks(ctx); err != nil {
		o.e.Log.Warn("delegation: reconcile after delegate", "err", err)
	}
	return json.Marshal(map[string]any{"tasks": made})
}

func (o *taskOps) prepareTemplateWorker(ctx context.Context, c actions.Call, parent *spec.Agent, task *store.NewTask) error {
	tpl, err := o.e.Store.GetTemplate(ctx, task.TemplateName, 0, true)
	if err != nil {
		return fmt.Errorf("template %s: %w", task.TemplateName, err)
	}
	if tpl.Status != "active" {
		return fmt.Errorf("template %s version %d is retired", tpl.Name, tpl.Version)
	}
	for _, prm := range tpl.Parameters {
		if prm.Mode == templates.UserPrompt {
			return fmt.Errorf("template %s has person-only parameter %q and cannot be delegated", tpl.Name, prm.Name)
		}
	}
	workerName, err := newTaskWorkerName()
	if err != nil {
		return err
	}
	_, rendered, err := o.e.renderTemplate(ctx, TemplateRequest{Template: tpl.Name, Version: tpl.Version, Name: workerName,
		Values: task.Parameters, ConversationID: c.ConversationID})
	if err != nil {
		return err
	}
	worker, err := spec.DecodeAgent(rendered)
	if err != nil {
		return err
	}
	allowed := make([]string, 0, len(worker.Connectors))
	for _, name := range worker.Connectors {
		if slices.Contains(parent.Connectors, name) {
			allowed = append(allowed, name)
		}
	}
	selected := allowed
	if task.Connectors != nil {
		selected = slices.Clone(task.Connectors)
		for _, name := range selected {
			if !slices.Contains(parent.Connectors, name) {
				return fmt.Errorf("connector %q is not enabled for the parent agent", name)
			}
			if !slices.Contains(worker.Connectors, name) {
				return fmt.Errorf("connector %q is not enabled by template %s", name, tpl.Name)
			}
		}
	}
	worker.Connectors = selected
	workerJSON, err := json.Marshal(worker)
	if err != nil {
		return err
	}
	defined, err := o.e.DefineAgent(ctx, DefineRequest{Spec: workerJSON, DryRun: true})
	if err != nil {
		return err
	}
	task.Agent = defined.Agent.Name
	task.TemplateVersion = tpl.Version
	task.WorkerSpec, err = json.Marshal(defined.Agent)
	if err != nil {
		return err
	}
	task.WorkerSHA = defined.Agent.SHA
	return nil
}

func newTaskWorkerName() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return "worker-" + hex.EncodeToString(id[:]), nil
}

func (e *Engine) taskWorkerAgent(ctx context.Context, snap *Snapshot, c *store.ClaimedRun, task *store.Task) (*spec.Agent, error) {
	doc, err := e.Store.TaskWorkerSpec(ctx, c.Agent, c.SpecSHA)
	if err != nil {
		return nil, err
	}
	worker, err := spec.DecodeAgent(doc)
	if err != nil {
		return nil, fmt.Errorf("template worker spec: %w", err)
	}
	if worker.Name != c.Agent {
		return nil, fmt.Errorf("template worker spec names %q, run names %q", worker.Name, c.Agent)
	}
	canonical, err := json.Marshal(worker)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(canonical)
	if got := hex.EncodeToString(sum[:]); got != c.SpecSHA {
		return nil, fmt.Errorf("template worker spec for %q does not match run spec %s", c.Agent, c.SpecSHA)
	}
	parent := snap.Config.Agents[task.ParentAgent]
	if parent == nil {
		return nil, fmt.Errorf("template worker parent agent %q is no longer configured", task.ParentAgent)
	}
	for _, name := range worker.Connectors {
		if !slices.Contains(parent.Connectors, name) {
			return nil, fmt.Errorf("template worker connector %q is outside parent agent %q's connector set", name, parent.Name)
		}
	}
	worker.Origin = spec.OriginDatabase
	worker.SHA = c.SpecSHA
	copyConfig := &spec.Config{Deployment: snap.Config.Deployment, Agents: make(map[string]*spec.Agent, len(snap.Config.Agents)+1)}
	for name, existing := range snap.Config.Agents {
		b, err := json.Marshal(existing)
		if err != nil {
			return nil, err
		}
		cloned, err := spec.DecodeAgent(b)
		if err != nil {
			return nil, err
		}
		cloned.Enabled, cloned.Origin, cloned.Version, cloned.SHA = existing.Enabled, existing.Origin, existing.Version, existing.SHA
		copyConfig.Agents[name] = cloned
	}
	copyConfig.Agents[worker.Name] = worker
	if err := copyConfig.Validate(); err != nil {
		return nil, fmt.Errorf("template worker %q no longer loads: %w", task.Template.Name, err)
	}
	return worker, nil
}

// MessageTask writes to a task: into its running reply when a chat agent's run works on it (a correction reaches the worker mid-turn), otherwise for its next run, which it wakes. The result's delivered says which.
func (o *taskOps) MessageTask(ctx context.Context, c actions.Call, p actions.TaskMessageParams) (json.RawMessage, error) {
	steerable := func(agent string) bool {
		a, ok := o.snap.Config.Agents[agent]
		return ok && a.Chat
	}
	got, err := o.e.Store.MessageTask(ctx, store.TaskMessage{FromRun: c.RunID, TaskID: p.TaskID, Body: strings.TrimSpace(p.Body), ReplyTo: p.ReplyTo,
		Steerable: steerable})
	if err != nil {
		return nil, err
	}
	if got.RunID != "" && got.Delivered == store.DeliveredQueued && o.e.Wake != nil {
		o.e.Wake()
	}
	return json.Marshal(got)
}

func (o *taskOps) CancelTask(ctx context.Context, c actions.Call, p actions.TaskCancelParams) (json.RawMessage, error) {
	if err := o.e.Store.CancelTask(ctx, p.TaskID, c.RunID, p.Reason, store.AgentAuthorPrefix+c.Agent); err != nil {
		return nil, err
	}
	if err := o.e.ReconcileTasks(ctx); err != nil {
		o.e.Log.Warn("delegation: reconcile after cancel", "err", err)
	}
	return json.RawMessage(`{"cancelled":true}`), nil
}

func (o *taskOps) RetryTask(ctx context.Context, c actions.Call, p actions.TaskRetryParams) (json.RawMessage, error) {
	a, ok := o.snap.Config.Agents[c.Agent]
	if !ok {
		return nil, fmt.Errorf("task:retry: agent %q is no longer configured", c.Agent)
	}
	// Checked again against the configuration in force, as delegate is.
	p, err := actions.DecodeTaskRetry(c.Params, a.DelegateTargets())
	if err != nil {
		return nil, err
	}
	got, err := o.e.Store.RetryTask(ctx, store.TaskRetry{FromRun: c.RunID, ActionID: c.ID, TaskID: p.TaskID, Reason: p.Reason,
		Brief: p.Brief, Agent: p.Agent, ByPerson: c.Rule == spec.RuleApprove})
	if err != nil {
		return nil, err
	}
	if err := o.e.ReconcileTasks(ctx); err != nil {
		o.e.Log.Warn("delegation: reconcile after retry", "err", err)
	}
	return json.Marshal(got)
}

// retryNeedsPerson reports whether a task:retry proposal is past the
// circuit breaker: its task failed MaxTaskFailures times, so only a
// person's approval may reopen it, whatever the rule.
func (e *Engine) retryNeedsPerson(ctx context.Context, params json.RawMessage) bool {
	var p actions.TaskRetryParams
	if json.Unmarshal(params, &p) != nil {
		return false
	}
	t, err := e.Store.GetTask(ctx, p.TaskID)
	return err == nil && t.Failures >= store.MaxTaskFailures
}

// taskContract is the system prompt's part for a run in a task's own
// conversation.
func taskContract(t *store.Task) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nThis conversation is a task (key %q) that agent %s delegated to you. %s is an agent, not a person: your result goes back to it, and it integrates your report with its other tasks' and answers its own person.\n", t.Key, t.ParentAgent, t.ParentAgent)
	b.WriteString("- Its brief is the first message from " + t.ParentAgent + " below; later messages from it add to the brief or answer you.\n")
	b.WriteString("- report: your complete answer to the brief, self-contained (it is all " + t.ParentAgent + " reads of your work). Lead with the outcome. The task is done when a run of yours ends succeeded without an open question.\n")
	b.WriteString("- outcome: succeeded when you did the work the brief asks for; failed when you could not do it and trying again as briefed would not help (the task fails, and " + t.ParentAgent + " may retry or reroute it); blocked when you cannot go on until something outside your reach changes (" + t.ParentAgent + " is told at once and may unblock you with a message). outcome_reason says why in one or two sentences; leave it empty only when you succeeded. Never report succeeded for work you did not do.\n")
	b.WriteString("- questions: only what you cannot proceed without. They go to " + t.ParentAgent + ", which answers or asks its person; your next run gets the answer.\n")
	b.WriteString("- notify: nothing you send here reaches a person directly; leave notify.send false.\n")
	if t.Fresh {
		b.WriteString("- You are a fresh worker for this task: you start with no memory, and nothing you put in memory is kept.\n")
	}
	if t.Template != nil {
		fmt.Fprintf(&b, "- Your worker spec was rendered from template %s version %d; your allowed connectors are limited to the task's pinned worker spec.\n", t.Template.Name, t.Template.Version)
	}
	return b.String()
}

// withOutcome adds outcome and outcome_reason to a task run's output
// schema. Both are required, as withPlan's field is, so a backend whose
// structured output needs every property listed (Codex) accepts them.
func withOutcome(schema json.RawMessage) json.RawMessage {
	var s map[string]any
	if json.Unmarshal(schema, &s) != nil {
		return schema
	}
	props, _ := s["properties"].(map[string]any)
	if props == nil {
		return schema
	}
	props["outcome"] = map[string]any{"type": "string", "enum": []string{store.OutcomeSucceeded, store.OutcomeFailed, store.OutcomeBlocked}}
	props["outcome_reason"] = map[string]any{"type": "string", "maxLength": 2000}
	req, _ := s["required"].([]any)
	for _, f := range []string{"outcome", "outcome_reason"} {
		if !slices.Contains(req, any(f)) {
			req = append(req, f)
		}
	}
	s["required"] = req
	b, _ := json.Marshal(s)
	return b
}

// tasksBudget bounds the delegated tasks a parent's prompt shows, and
// taskReportMax one report in it: room for a batch of full reports beside
// the conversation (conversationBudget). A report clipped here is whole on
// GET /tasks/{id}.
const (
	tasksBudget   = 48 << 10
	taskReportMax = 12 << 10
)

// tasksSection is the prompt's part about the tasks this conversation
// delegated: every task's state, and in full what is new since the
// conversation's previous run (a report, a question, a failure). Newest
// batches are kept when the budget runs out.
func tasksSection(tasks []store.Task, loc *time.Location) string {
	if len(tasks) == 0 {
		return ""
	}
	var blocks []string
	used := 0
	for i := len(tasks) - 1; i >= 0; i-- {
		t := tasks[i]
		var b strings.Builder
		fmt.Fprintf(&b, "## %s: %s, %s\n\ntask_id: %s\nagent: %s\n", t.Key, t.Status, newLabel(t), t.ID, t.Agent)
		if len(t.DependsOn) > 0 {
			fmt.Fprintf(&b, "after: %s\n", strings.Join(t.DependsOn, ", "))
		}
		if t.Decision != nil {
			fmt.Fprintf(&b, "decision: %q, %s\n", t.Decision.Question, t.Decision.Status)
		}
		if t.Outcome != nil && *t.Outcome != store.OutcomeSucceeded {
			fmt.Fprintf(&b, "outcome: %s: %s\n", *t.Outcome, t.OutcomeReasonText())
		}
		if t.Error != nil {
			fmt.Fprintf(&b, "error: %s\n", *t.Error)
		}
		if t.Attempt > 1 || t.Failures > 0 {
			fmt.Fprintf(&b, "attempt: %d, failed %d of at most %d times\n", t.Attempt, t.Failures, store.MaxTaskFailures)
		}
		if t.StalledAt != nil {
			b.WriteString("stalled: nothing in its batch can move without you; retry or cancel what it waits for, or ask your person about its decision\n")
		}
		if t.Status == store.TaskFailed && t.Failures >= store.MaxTaskFailures {
			b.WriteString("retry: only a person's approval reopens it now\n")
		}
		for _, q := range t.Questions {
			fmt.Fprintf(&b, "question (reply_to %s): %s\n", q.ID, q.Body)
			if len(q.Fields) > 0 {
				fmt.Fprintf(&b, "its form (answer each in your message):\n%s", forms.Describe(q.Fields))
			}
		}
		if t.Report != nil {
			fmt.Fprintf(&b, "summary: %s\n", t.Report.Summary)
			if t.New {
				fmt.Fprintf(&b, "\n%s\n", clip(strings.TrimSpace(t.Report.Report), taskReportMax))
			}
		}
		if t.FinishedAt != nil {
			fmt.Fprintf(&b, "finished: %s\n", t.FinishedAt.In(loc).Format("2006-01-02 15:04"))
		}
		s := b.String() + "\n"
		if used+len(s) > tasksBudget {
			blocks = append(blocks, fmt.Sprintf("(%d earlier tasks not shown)\n", i+1))
			break
		}
		used += len(s)
		blocks = append(blocks, s)
	}
	var out strings.Builder
	out.WriteString("\n# Tasks you delegated from this conversation (oldest first)\n\n")
	out.WriteString("Their agents worked on them in their own conversations. Integrate the reports into your answer; reply to a task's question with task:message (reply_to its id), or ask your person if only they can answer. A failed task's dependents wait for it: retry it with task:retry (a new brief or another agent when that would help), or cancel them. You are woken again when tasks still open settle, fail or ask; do not poll.\n\n")
	for i := len(blocks) - 1; i >= 0; i-- {
		out.WriteString(blocks[i])
	}
	return out.String()
}

func newLabel(t store.Task) string {
	if t.New {
		return "new since your last run"
	}
	return "as before"
}

// messageFrom names who wrote a message in a prompt: the person, or the
// agent that delegated the conversation's task.
func messageFrom(m store.Message) string {
	if m.Author != nil && strings.HasPrefix(*m.Author, store.AgentAuthorPrefix) {
		return "from " + strings.TrimPrefix(*m.Author, store.AgentAuthorPrefix)
	}
	return ""
}
