package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// The schedule actions (actions.ScheduleTypes): a typed change to one agent's triggers, applied as a new version through DefineAgent, so it is revertible and carries the run and the person like any agent:define; or, for run:now, a run enqueued now.

// definedSpec is the spec a change to name starts from, and the version it must still be at when the change is stored: a database agent's newest definition, or a built-in agent as it loads (version 0: the change defines it in the database). A file agent is read-only.
func (e *Engine) definedSpec(ctx context.Context, name string) (*spec.Agent, int, error) {
	cur := e.Current().Config.Agents[name]
	if cur == nil {
		return nil, 0, fmt.Errorf("agent %q: %w", name, store.ErrNotFound)
	}
	switch cur.Origin {
	case spec.OriginBuiltin:
		b, err := json.Marshal(cur)
		if err != nil {
			return nil, 0, err
		}
		a, err := spec.DecodeAgent(b)
		return a, 0, err
	case spec.OriginDatabase:
		d, err := e.Store.LatestDefinition(ctx, name)
		if err != nil {
			return nil, 0, err
		}
		a, err := spec.DecodeAgent(d.Spec)
		return a, d.Version, err
	}
	return nil, 0, fmt.Errorf("%w: agent %q is defined in the configuration files; change it there", ErrReadOnly, name)
}

// planSchedule is what a schedule action of proposer would do at now: the changed spec, the version it applies to, and the result to echo. Nothing is stored.
func (e *Engine) planSchedule(ctx context.Context, proposer, typ string, p actions.ScheduleParams, now time.Time) (*spec.Agent, int, *actions.ScheduleResult, error) {
	cfg := e.Current().Config
	from := cfg.Agents[proposer]
	if from == nil {
		return nil, 0, nil, fmt.Errorf("agent %q is no longer configured", proposer)
	}
	target := p.Agent
	if target == "" {
		target = proposer
	}
	if !actions.MayTarget(from, target) {
		return nil, 0, nil, fmt.Errorf("%s: %s may change only its own schedules or those of agents it delegates to, not %q", typ, proposer, target)
	}
	loaded := cfg.Agents[target]
	if loaded == nil {
		return nil, 0, nil, fmt.Errorf("%s: agent %q: %w", typ, target, store.ErrNotFound)
	}
	if typ == actions.RunNowType {
		return loaded, 0, nil, nil
	}
	a, version, err := e.definedSpec(ctx, target)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("%s: %w", typ, err)
	}
	res, err := actions.ApplySchedule(a, loaded.Timezone, typ, p, now)
	if err != nil {
		return nil, 0, nil, err
	}
	return a, version, res, nil
}

// PreviewSchedule is what a proposed schedule action would do now, for the person deciding it and the model proposing it; nil for run:now.
func (e *Engine) PreviewSchedule(ctx context.Context, proposer, typ string, params json.RawMessage) (*actions.ScheduleResult, error) {
	p, err := actions.DecodeSchedule(typ, params)
	if err != nil {
		return nil, err
	}
	_, _, res, err := e.planSchedule(ctx, proposer, typ, p, time.Now())
	return res, err
}

// scheduleAction executes a schedule action or run:now for the call's run.
func (e *Engine) scheduleAction(ctx context.Context, c actions.Call, p actions.ScheduleParams) (json.RawMessage, error) {
	a, version, res, err := e.planSchedule(ctx, c.Agent, c.Type, p, time.Now())
	if err != nil {
		return nil, err
	}
	if c.Type == actions.RunNowType {
		return e.runNow(ctx, c, a, p.Prompt)
	}
	// Triggers are within an agent's reach (spec.Widening never names them); this holds that if either changes.
	if w := spec.Widening(e.Current().Config.Agents[a.Name], a); len(w) > 0 && c.Rule != spec.RuleApprove {
		return nil, fmt.Errorf("%s: the change widens the agent (%s), so it needs a person's approval", c.Type, strings.Join(w, "; "))
	}
	who, err := e.Store.RunInstructor(ctx, c.RunID)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	d, err := e.DefineAgent(ctx, DefineRequest{Name: a.Name, Spec: b, Author: who, RunID: c.RunID, Upsert: true, Base: version})
	if err != nil {
		return nil, err
	}
	res.Version = d.Definition.Version
	return json.Marshal(res)
}

// runNow enqueues a manual run of a. A run its person's message did not wake may not start its own agent again: that is continue, which is bounded.
func (e *Engine) runNow(ctx context.Context, c actions.Call, a *spec.Agent, prompt string) (json.RawMessage, error) {
	if a.Name == c.Agent {
		r, err := e.Store.GetRun(ctx, c.RunID)
		if err != nil {
			return nil, err
		}
		if !Attended(r.Trigger) {
			return nil, fmt.Errorf("%s: a %s run cannot start %s again; use continue", actions.RunNowType, r.Trigger, a.Name)
		}
	}
	id, err := e.Store.EnqueueRun(ctx, store.NewRun{Agent: a.Name, SpecSHA: a.SHA, Trigger: "manual", Prompt: prompt})
	if err != nil {
		return nil, err
	}
	if e.Wake != nil {
		e.Wake()
	}
	return json.Marshal(map[string]any{"agent": a.Name, "run_id": id})
}

// PreviewAction is what a pending action would do if it executed now, computed by code for the person deciding it (GET /actions/{id}, the approval page): for a schedule action, its result as executing it would echo it; for agent:define, a dry run with the spec's diff (previewDefine). nil for a type with nothing to preview or an action already decided.
func (e *Engine) PreviewAction(ctx context.Context, act *store.Action) (json.RawMessage, error) {
	if act.Status != "pending" {
		return nil, nil
	}
	if act.Type == actions.DefineType {
		return e.previewDefine(ctx, act.Params)
	}
	if !actions.IsScheduleType(act.Type) || act.Type == actions.RunNowType {
		return nil, nil
	}
	res, err := e.PreviewSchedule(ctx, act.Agent, act.Type, act.Params)
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}
