package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/kensac/orbit/internal/engine"
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

type agentChange struct {
	Spec json.RawMessage `json:"spec"`
	// DryRun validates without storing.
	DryRun bool `json:"dry_run"`
}

func (s *Server) createAgent(w http.ResponseWriter, r *http.Request) {
	var body agentChange
	if !decode(w, r, maxSpecBytes, &body, bodyRequired) {
		return
	}
	res, err := s.defineAgent(r.Context(), engine.DefineRequest{Spec: body.Spec, DryRun: body.DryRun, Author: actor(r, "")})
	s.respondDefine(w, http.StatusCreated, res, err)
}

func (s *Server) updateAgent(w http.ResponseWriter, r *http.Request) {
	var body agentChange
	if !decode(w, r, maxSpecBytes, &body, bodyRequired) {
		return
	}
	res, err := s.defineAgent(r.Context(), engine.DefineRequest{Name: r.PathValue("name"), Spec: body.Spec, DryRun: body.DryRun,
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

// DefineResult is what a definition change did, as the API answers it.
type DefineResult struct {
	Valid bool `json:"valid"`
	// Agent is the agent as it loads, defaults applied.
	Agent      *spec.Agent            `json:"spec,omitempty"`
	Definition *store.AgentDefinition `json:"definition,omitempty"`
	NextFires  []scheduler.NextFire   `json:"next_fires,omitempty"`
	Row        *store.AgentRow        `json:"agent,omitempty"`
}

func (s *Server) defineResult(ctx context.Context, d *engine.Defined, err error) (*DefineResult, error) {
	if err != nil {
		return nil, err
	}
	res := &DefineResult{Valid: true, Agent: d.Agent, Definition: d.Definition, NextFires: scheduler.NextFires(d.Agent, time.Now())}
	if d.Definition != nil {
		res.Row = s.agentRow(ctx, d.Definition.Name)
	}
	return res, nil
}

func (s *Server) defineAgent(ctx context.Context, req engine.DefineRequest) (*DefineResult, error) {
	d, err := s.Engine.DefineAgent(ctx, req)
	return s.defineResult(ctx, d, err)
}

func (s *Server) changeAgentState(ctx context.Context, name, change, author string) (*DefineResult, error) {
	d, err := s.Engine.ChangeAgentState(ctx, name, change, author, "", 0)
	return s.defineResult(ctx, d, err)
}

// revertAgent is POST /agents/{name}/definitions/{version}/revert.
func (s *Server) revertAgent(w http.ResponseWriter, r *http.Request) {
	v, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || v < 1 {
		writeErr(w, http.StatusBadRequest, "version: want a positive integer")
		return
	}
	d, err := s.Engine.ChangeAgentState(r.Context(), r.PathValue("name"), "revert", actor(r, ""), "", v)
	res, err := s.defineResult(r.Context(), d, err)
	s.respondDefine(w, http.StatusOK, res, err)
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
	case errors.Is(err, engine.ErrReadOnly):
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
