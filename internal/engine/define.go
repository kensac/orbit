package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// Defining agents in the database (docs/api.md, Defining agents): the API's
// path and an agent's own agent:define action share it, so both are held to
// the same loader. A change is validated by loading the whole configuration
// with it, exactly as it will be put in force, then stored as a new version
// and reloaded.

// ErrReadOnly is a change to an agent the database does not own (a file
// agent, or a built-in one changed other than by defining it).
var ErrReadOnly = errors.New("read-only")

// DefineRequest is one create or update of a database agent.
type DefineRequest struct {
	// Name is the agent an update changes; a create takes it from Spec.
	Name   string
	Spec   json.RawMessage
	DryRun bool
	// Author is the person who made the change, or on whose instruction
	// an agent made it.
	Author string
	// RunID is the agent's run, when an agent makes the change.
	RunID string
	// Update changes an existing agent; Upsert creates it when the
	// database does not define it yet (agent:define).
	Update bool
	Upsert bool
}

// Defined is what a definition change did.
type Defined struct {
	// Agent is the agent as it loads, defaults applied.
	Agent      *spec.Agent
	Definition *store.AgentDefinition
}

func (e *Engine) reload(ctx context.Context) error {
	if e.Reloader != nil {
		return e.Reloader(ctx)
	}
	return e.Reload(ctx)
}

// DefineAgent validates and, unless DryRun, stores and reloads one agent
// definition.
func (e *Engine) DefineAgent(ctx context.Context, req DefineRequest) (*Defined, error) {
	a, err := spec.DecodeAgent(req.Spec)
	if err != nil || len(bytes.TrimSpace(req.Spec)) == 0 {
		return nil, fmt.Errorf("%w: spec: %s", store.ErrInvalid, errOr(err, "required"))
	}
	if req.Update || req.Upsert {
		if a.Name == "" {
			a.Name = req.Name
		}
		if req.Name != "" && a.Name != req.Name {
			return nil, fmt.Errorf("%w: spec names %q, the path %q", store.ErrInvalid, a.Name, req.Name)
		}
	}
	if a.Name == "" {
		return nil, fmt.Errorf("%w: spec.name is required", store.ErrInvalid)
	}
	cur := e.Current().Config.Agents[a.Name]
	if cur != nil && cur.Origin == spec.OriginFile {
		return nil, fmt.Errorf("%w: agent %q is defined in the configuration files; change it there", ErrReadOnly, a.Name)
	}
	update := req.Update
	if req.Upsert {
		update = cur != nil
	}
	after, change, enabled := 0, "create", true
	if update {
		change = "update"
		prev, err := e.Store.LatestDefinition(ctx, a.Name)
		switch {
		case errors.Is(err, store.ErrNotFound) && cur != nil && cur.Origin == spec.OriginBuiltin:
			// The first change to a built-in agent defines it in the
			// database, which then replaces the built-in.
			change = "create"
		case err != nil:
			return nil, err
		case prev.Archived:
			return nil, fmt.Errorf("agent %q is archived: %w", a.Name, store.ErrNotFound)
		default:
			after, enabled = prev.Version, prev.Enabled
		}
	} else if cur != nil && cur.Origin == spec.OriginDatabase {
		return nil, fmt.Errorf("agent %q is already defined; update it with PUT: %w", a.Name, store.ErrConflict)
	}
	if a.Enabled != nil {
		enabled = *a.Enabled
		a.Enabled = nil
	}
	canon, _ := json.Marshal(a)
	cfg, err := e.LoadConfig(ctx, map[string]*spec.Definition{a.Name: {
		Name: a.Name, Version: after + 1, Enabled: enabled, Spec: canon}})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	res := &Defined{Agent: cfg.Agents[a.Name]}
	if req.DryRun {
		return res, nil
	}
	d, err := e.Store.PutDefinition(ctx, store.NewDefinition{Name: a.Name, Spec: canon, Enabled: enabled,
		Change: change, Author: req.Author, RunID: req.RunID, After: after})
	if err != nil {
		return nil, err
	}
	res.Definition = d
	if err := e.reload(ctx); err != nil {
		return nil, fmt.Errorf("stored version %d, but the reload failed: %w", d.Version, err)
	}
	res.Agent = e.Current().Config.Agents[a.Name]
	return res, nil
}

// ChangeAgentState enables, disables or archives a database agent, or
// reverts it to an earlier version (revert > 0): a new version either way.
func (e *Engine) ChangeAgentState(ctx context.Context, name, change, author, runID string, revert int) (*Defined, error) {
	cur := e.Current().Config.Agents[name]
	if cur != nil && cur.Origin != spec.OriginDatabase {
		return nil, fmt.Errorf("%w: agent %q is defined in %s, not the database", ErrReadOnly, name, cur.Origin)
	}
	prev, err := e.Store.LatestDefinition(ctx, name)
	if err != nil {
		return nil, err
	}
	if prev.Archived {
		return nil, fmt.Errorf("agent %q is archived: %w", name, store.ErrNotFound)
	}
	specJSON, enabled, archived := prev.Spec, prev.Enabled, change == "archive"
	switch change {
	case "enable":
		enabled = true
	case "disable":
		enabled = false
	case "revert":
		old, err := e.Store.Definition(ctx, name, revert)
		if err != nil {
			return nil, err
		}
		if old.Archived {
			return nil, fmt.Errorf("%w: version %d archived the agent; revert to a version that defines it", store.ErrInvalid, revert)
		}
		specJSON, enabled = old.Spec, old.Enabled
	}
	override := &spec.Definition{Name: name, Version: prev.Version + 1, Enabled: enabled, Spec: specJSON}
	if archived {
		override = nil
	}
	if _, err := e.LoadConfig(ctx, map[string]*spec.Definition{name: override}); err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	d, err := e.Store.PutDefinition(ctx, store.NewDefinition{Name: name, Spec: specJSON, Enabled: enabled,
		Archived: archived, Change: change, Author: author, RunID: runID, After: prev.Version, Reverts: revert})
	if err != nil {
		return nil, err
	}
	if err := e.reload(ctx); err != nil {
		return nil, fmt.Errorf("stored version %d, but the reload failed: %w", d.Version, err)
	}
	return &Defined{Definition: d, Agent: e.Current().Config.Agents[name]}, nil
}

func errOr(err error, s string) string {
	if err != nil {
		return err.Error()
	}
	return s
}

// defineWidening is how an agent:define with params would widen the agent it names past its current version (spec.Widening), from a dry run of the change. A change that does not load widens nothing here; executing it fails the same way.
func (e *Engine) defineWidening(ctx context.Context, params json.RawMessage) []string {
	sp, err := actions.DecodeDefine(params)
	if err != nil {
		return nil
	}
	d, err := e.DefineAgent(ctx, DefineRequest{Spec: sp, Upsert: true, DryRun: true})
	if err != nil {
		return nil
	}
	return spec.Widening(e.Current().Config.Agents[d.Agent.Name], d.Agent)
}

// defineAction executes agent:define: the change is the run's, made for the
// person whose message the run answered. An action executing by rule alone
// is checked for widening again against the agent as it is now, since a
// person may have changed it after the proposal.
func (e *Engine) defineAction(ctx context.Context, c actions.Call, specJSON json.RawMessage) (json.RawMessage, error) {
	if c.Rule != spec.RuleApprove {
		params, _ := json.Marshal(map[string]json.RawMessage{"spec": specJSON})
		if w := e.defineWidening(ctx, params); len(w) > 0 {
			return nil, fmt.Errorf("agent:define: the change widens the agent (%s), so it needs a person's approval; propose it again", strings.Join(w, "; "))
		}
	}
	who, err := e.Store.RunInstructor(ctx, c.RunID)
	if err != nil {
		return nil, err
	}
	d, err := e.DefineAgent(ctx, DefineRequest{Spec: specJSON, Author: who, RunID: c.RunID, Upsert: true})
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"agent": d.Definition.Name, "version": d.Definition.Version, "change": d.Definition.Change})
}
