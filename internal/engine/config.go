package engine

import (
	"context"
	"encoding/json"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// Definitions reads the database's agent definitions in force, as the
// loader takes them.
func Definitions(ctx context.Context, s *store.Store) ([]spec.Definition, error) {
	rows, err := s.ActiveDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]spec.Definition, 0, len(rows))
	for _, d := range rows {
		out = append(out, spec.Definition{Name: d.Name, Version: d.Version, Enabled: d.Enabled, Spec: d.Spec})
	}
	return out, nil
}

// LoadConfig loads the configuration orbit runs: e.ConfigDir's files, the
// database's definitions and the built-in agents. override replaces or adds
// definitions by name before loading (a nil value removes one), so a change
// is validated exactly as it would load, without being stored.
func (e *Engine) LoadConfig(ctx context.Context, override map[string]*spec.Definition) (*spec.Config, error) {
	defs, err := Definitions(ctx, e.Store)
	if err != nil {
		return nil, err
	}
	var merged []spec.Definition
	for _, d := range defs {
		if _, ok := override[d.Name]; !ok {
			merged = append(merged, d)
		}
	}
	for _, d := range override {
		if d != nil {
			merged = append(merged, *d)
		}
	}
	return spec.LoadAll(e.ConfigDir, merged)
}

// Reload loads the configuration and puts it in force: every agent and spec
// recorded, channels built. A configuration that fails to load changes
// nothing.
func (e *Engine) Reload(ctx context.Context) error {
	cfg, err := e.LoadConfig(ctx, nil)
	if err != nil {
		return err
	}
	if err := SyncAgents(ctx, e.Store, cfg); err != nil {
		return err
	}
	return e.SetConfig(cfg)
}

// SyncAgents records a loaded configuration's agents and their specs.
func SyncAgents(ctx context.Context, s *store.Store, cfg *spec.Config) error {
	specs := map[string]store.AgentSpec{}
	for name, a := range cfg.Agents {
		b, err := json.Marshal(a)
		if err != nil {
			return err
		}
		specs[name] = store.AgentSpec{SHA: a.SHA, JSON: b, Enabled: a.IsEnabled()}
	}
	return s.SyncAgents(ctx, specs)
}
