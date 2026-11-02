package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/kensac/orbit/internal/scheduler"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// Defining agents through the API (docs/api.md, Defining agents). A change
// is validated by loading the whole configuration with it, exactly as it
// would be put in force, then stored as a new version and reloaded.

// maxSpecBytes bounds an agent definition: a charter of a few pages plus
// its sources and rules, well inside the memory documents it will write.
const maxSpecBytes = 256 << 10

// errReadOnly is a change to an agent the API does not own.
var errReadOnly = errors.New("read-only")

type agentChange struct {
	Spec json.RawMessage `json:"spec"`
	// DryRun validates without storing.
	DryRun bool `json:"dry_run"`
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var body agentChange
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSpecBytes)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.DefineAgent(r.Context(), DefineRequest{Spec: body.Spec, DryRun: body.DryRun, Author: actor(r, "")})
	s.respondDefine(w, http.StatusCreated, res, err)
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	var body agentChange
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSpecBytes)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.DefineAgent(r.Context(), DefineRequest{Name: r.PathValue("name"), Spec: body.Spec, DryRun: body.DryRun,
		Author: actor(r, ""), Update: true})
	s.respondDefine(w, http.StatusOK, res, err)
}

func (s *Server) setAgentState(change string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := s.changeAgentState(r.Context(), r.PathValue("name"), change, actor(r, ""))
		s.respondDefine(w, http.StatusOK, res, err)
	}
}

func (s *Server) agentDefinitions(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	h, err := s.Store.DefinitionHistory(r.Context(), name)
	if err != nil {
		respond(w, nil, err)
		return
	}
	origin := ""
	if a, ok := s.Engine.Current().Config.Agents[name]; ok {
		origin = a.Origin
	}
	if origin == "" && len(h) == 0 {
		respond(w, nil, store.ErrNotFound)
		return
	}
	respond(w, map[string]any{"name": name, "origin": origin, "versions": orEmpty(h)}, nil)
}

// DefineRequest is one create or update of a database agent.
type DefineRequest struct {
	// Name is the agent an update changes; a create takes it from Spec.
	Name   string
	Spec   json.RawMessage
	DryRun bool
	Author string
	Update bool
}

// DefineResult is what a definition change did.
type DefineResult struct {
	Valid bool `json:"valid"`
	// Agent is the agent as it loads, defaults applied.
	Agent      *spec.Agent            `json:"spec,omitempty"`
	Definition *store.AgentDefinition `json:"definition,omitempty"`
	NextFires  []scheduler.NextFire   `json:"next_fires,omitempty"`
	Row        *store.AgentRow        `json:"agent,omitempty"`
}

// DefineAgent validates and, unless DryRun, stores and reloads one agent
// definition. It is the API's path and the general agent's agent:define
// action's, so both are held to the same loader.
func (s *Server) DefineAgent(ctx context.Context, req DefineRequest) (*DefineResult, error) {
	a, err := spec.DecodeAgent(req.Spec)
	if err != nil || len(bytes.TrimSpace(req.Spec)) == 0 {
		return nil, fmt.Errorf("%w: spec: %v", store.ErrInvalid, errOr(err, "required"))
	}
	if req.Update {
		if a.Name == "" {
			a.Name = req.Name
		}
		if a.Name != req.Name {
			return nil, fmt.Errorf("%w: spec names %q, the path %q", store.ErrInvalid, a.Name, req.Name)
		}
	}
	if a.Name == "" {
		return nil, fmt.Errorf("%w: spec.name is required", store.ErrInvalid)
	}
	cur := s.Engine.Current().Config.Agents[a.Name]
	if cur != nil && cur.Origin == spec.OriginFile {
		return nil, fmt.Errorf("%w: agent %q is defined in the configuration files; change it there", errReadOnly, a.Name)
	}
	after, change, enabled := 0, "create", true
	if req.Update {
		change = "update"
		prev, err := s.Store.LatestDefinition(ctx, a.Name)
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
	cfg, err := s.Engine.LoadConfig(ctx, map[string]*spec.Definition{a.Name: {
		Name: a.Name, Version: after + 1, Enabled: enabled, Spec: canon}})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	res := &DefineResult{Valid: true, Agent: cfg.Agents[a.Name], NextFires: scheduler.NextFires(cfg.Agents[a.Name], time.Now())}
	if req.DryRun {
		return res, nil
	}
	d, err := s.Store.PutDefinition(ctx, store.NewDefinition{Name: a.Name, Spec: canon, Enabled: enabled,
		Change: change, Author: req.Author, After: after})
	if err != nil {
		return nil, err
	}
	res.Definition = d
	if err := s.reload(ctx); err != nil {
		return nil, fmt.Errorf("stored version %d, but the reload failed: %w", d.Version, err)
	}
	res.Agent = s.Engine.Current().Config.Agents[a.Name]
	res.Row = s.agentRow(ctx, a.Name)
	return res, nil
}

// changeAgentState enables, disables or archives a database agent: a new
// version with the same spec.
func (s *Server) changeAgentState(ctx context.Context, name, change, author string) (*DefineResult, error) {
	cur := s.Engine.Current().Config.Agents[name]
	if cur != nil && cur.Origin != spec.OriginDatabase {
		return nil, fmt.Errorf("%w: agent %q is defined in %s, not the database", errReadOnly, name, cur.Origin)
	}
	prev, err := s.Store.LatestDefinition(ctx, name)
	if err != nil {
		return nil, err
	}
	if prev.Archived {
		return nil, fmt.Errorf("agent %q is archived: %w", name, store.ErrNotFound)
	}
	enabled, archived := prev.Enabled, change == "archive"
	switch change {
	case "enable":
		enabled = true
	case "disable":
		enabled = false
	}
	override := &spec.Definition{Name: name, Version: prev.Version + 1, Enabled: enabled, Spec: prev.Spec}
	if archived {
		override = nil
	}
	if _, err := s.Engine.LoadConfig(ctx, map[string]*spec.Definition{name: override}); err != nil {
		return nil, fmt.Errorf("%w: %w", store.ErrInvalid, err)
	}
	d, err := s.Store.PutDefinition(ctx, store.NewDefinition{Name: name, Spec: prev.Spec, Enabled: enabled,
		Archived: archived, Change: change, Author: author, After: prev.Version})
	if err != nil {
		return nil, err
	}
	if err := s.reload(ctx); err != nil {
		return nil, fmt.Errorf("stored version %d, but the reload failed: %w", d.Version, err)
	}
	res := &DefineResult{Valid: true, Definition: d, Agent: s.Engine.Current().Config.Agents[name]}
	res.NextFires = scheduler.NextFires(res.Agent, time.Now())
	res.Row = s.agentRow(ctx, name)
	return res, nil
}

func (s *Server) reload(ctx context.Context) error {
	if s.Reload != nil {
		return s.Reload(ctx)
	}
	return s.Engine.Reload(ctx)
}

func (s *Server) agentRow(ctx context.Context, name string) *store.AgentRow {
	rows, err := s.Store.ListAgents(ctx)
	if err != nil {
		return nil
	}
	s.withOrigin(rows)
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}

func (s *Server) respondDefine(w http.ResponseWriter, status int, res *DefineResult, err error) {
	switch {
	case errors.Is(err, errReadOnly):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil:
		respond(w, nil, err)
	case res.Definition == nil:
		respond(w, res, nil)
	default:
		respondStatus(w, status, res, nil)
	}
}

func errOr(err error, s string) string {
	if err != nil {
		return err.Error()
	}
	return s
}
