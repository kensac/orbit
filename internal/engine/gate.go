package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path"
	"time"

	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/metrics"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// gateSkip is a gate's verdict that a run has nothing to judge: the run
// ends skipped, naming the gate, and no backend is called.
type gateSkip struct {
	gate, summary string
}

func (g *gateSkip) Error() string { return "skipped by gate " + g.gate + ": " + g.summary }

// gated reports whether a run's gates apply: a scheduled run with no
// person's message waiting. A message, a manual run, a continuation, a
// build and a recovered attempt always reach the backend, because someone
// asked for judgment (or already paid for part of it).
func gated(a *spec.Agent, c *store.ClaimedRun, rc *store.RunContext, recovering bool) bool {
	return len(a.Gates) > 0 && c.Trigger == "schedule" && len(rc.Messages) == 0 && !recovering
}

// gate runs the agent's gates in order over what the model would be shown
// (inputs) and every document fetched or computed (docs, by source or
// transform name), and returns the first that ends the run, or nil.
func (e *Engine) gate(ctx context.Context, a *spec.Agent, inputs []runner.Input, docs map[string][]byte) (*gateSkip, error) {
	for i, g := range a.Gates {
		switch g.Kind {
		case "unchanged":
			last, err := e.Store.LastSucceededInputs(ctx, a.Name)
			if err != nil {
				return nil, err
			}
			if last == nil || last.SpecSHA != a.SHA {
				continue
			}
			now := map[string]string{}
			for _, in := range inputs {
				sum := sha256.Sum256(in.Content)
				now[path.Base(in.Path)] = hex.EncodeToString(sum[:])
			}
			if len(now) > 0 && maps.Equal(now, last.Inputs) {
				return &gateSkip{gate: g.Kind, summary: fmt.Sprintf(
					"inputs unchanged since run %s, the last that succeeded", last.RunID)}, nil
			}
		case "items_above":
			doc, err := harness.Decode(docs[g.Input])
			if err != nil {
				return nil, fmt.Errorf("gate %d: %s is not JSON: %w", i, g.Input, err)
			}
			records, err := harness.Records(g.Input, doc, g.Items)
			if err != nil {
				return nil, fmt.Errorf("gate %d: %w", i, err)
			}
			if len(records) <= g.N {
				return &gateSkip{gate: g.Kind, summary: fmt.Sprintf(
					"%s has %d records, not above %d", g.Input, len(records), g.N)}, nil
			}
		}
	}
	return nil, nil
}

// skip ends a run a gate stopped: skipped, naming the gate, with no
// notification (a quiet day sends nothing, as it costs nothing).
func (e *Engine) skip(ctx context.Context, snap *Snapshot, a *spec.Agent, c *store.ClaimedRun, start time.Time, g *gateSkip) error {
	rr := store.RunResult{Attempt: c.Attempt, Status: store.StatusSkipped, Gate: g.gate,
		Summary: "Skipped: " + g.summary, Report: "orbit did not call the model: " + g.summary + "."}
	if err := e.Store.FinishRun(ctx, c.ID, rr); err != nil {
		return err
	}
	metrics.RunSkipped(a.Name, g.gate)
	metrics.RunFinished(a.Name, store.StatusSkipped, runnerKind(snap, a), time.Since(start), nil)
	return nil
}
