package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/transforms"
)

// transform runs the agent's transforms over the fetched sources (raw, by
// name), in spec order. Each output is an input of its own, written and
// stored like a source's, so the model, a recovered attempt and the
// figure check all see it as one. A transform that fails fails the run,
// naming it: a model shown half its inputs would judge the wrong thing.
func (e *Engine) transform(ctx context.Context, a *spec.Agent, c *store.ClaimedRun, workdir, inputsDir string,
	raw map[string][]byte, inputs []runner.Input, emit func(string, any)) ([]runner.Input, error) {
	for _, t := range a.Transforms {
		params, err := json.Marshal(t.Params)
		if err != nil {
			return nil, err
		}
		in, ok := raw[t.Source]
		if !ok {
			// A webhook source on a run no delivery started: there is nothing to reduce, and no input of this name.
			emit("orbit.transform_skipped", map[string]any{"transform": t.Name, "source": t.Source, "reason": "the source is absent"})
			continue
		}
		out, err := transforms.Apply(t.Kind, params, in)
		if err != nil {
			emit("orbit.transform_failed", map[string]any{"transform": t.Name, "source": t.Source, "error": err.Error()})
			return nil, fmt.Errorf("transform %s (%s of source %s): %w", t.Name, t.Kind, t.Source, err)
		}
		rel := filepath.Join(inputsDir, t.Name+".json")
		if err := os.WriteFile(filepath.Join(workdir, rel), out, 0o600); err != nil {
			return nil, err
		}
		if err := e.Store.PutArtifact(ctx, c.ID, c.Attempt, "in", rel, "application/json", out); err != nil {
			return nil, err
		}
		emit("orbit.transform", map[string]any{"transform": t.Name, "kind": t.Kind, "source": t.Source,
			"bytes_in": len(in), "bytes_out": len(out)})
		inputs = append(inputs, runner.Input{Name: t.Name, Path: rel, MediaType: "application/json", Content: out})
	}
	return inputs, nil
}
