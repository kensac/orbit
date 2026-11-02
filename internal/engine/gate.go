package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path"
	"path/filepath"
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

// gated reports whether a run's gates apply: a scheduled or webhook run
// with no person's message waiting. A message, a manual run, a
// continuation, a build and a recovered attempt always reach the backend,
// because someone asked for judgment (or already paid for part of it).
func gated(a *spec.Agent, c *store.ClaimedRun, rc *store.RunContext, recovering bool) bool {
	return len(a.Gates) > 0 && (c.Trigger == "schedule" || c.Trigger == store.TriggerWebhook) && len(rc.Messages) == 0 && !recovering
}

// gate runs the agent's gates in order over what the model would be shown
// (inputs) and every document fetched or computed (docs, by source or
// transform name), and returns the first that ends the run, or nil. A
// precheck runs its command in the workdir, where the inputs are written.
func (e *Engine) gate(ctx context.Context, a *spec.Agent, c *store.ClaimedRun, workdir, inputsDir string, inputs []runner.Input, docs map[string][]byte, emit func(string, any)) (*gateSkip, error) {
	for i, g := range a.Gates {
		switch g.Kind {
		case "precheck":
			sk, err := e.precheck(ctx, a, g, c.ID, workdir, filepath.Join(workdir, inputsDir), emit)
			if err != nil || sk != nil {
				return sk, err
			}
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
			raw, ok := docs[g.Input]
			if !ok {
				// A webhook source (or a transform of one) on a run no delivery started.
				return &gateSkip{gate: g.Kind, summary: g.Input + " is absent: no webhook delivery started this run"}, nil
			}
			doc, err := harness.Decode(raw)
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
	if g.gate == store.GateMissed {
		// The agent did no work, so it is not a success for staleness.
		metrics.RunMissed(a.Name, time.Since(start))
		return nil
	}
	metrics.RunFinished(a.Name, store.StatusSkipped, runnerKind(snap, a), time.Since(start), nil)
	return nil
}

// missed is the verdict on a schedule fire claimed later than its grace (spec.MissedGraceFor): the run ends skipped, gate missed, before anything is read or fetched. Only a first attempt is judged: a later one continues work an attempt that started in time began.
func missed(a *spec.Agent, c *store.ClaimedRun, now time.Time) *gateSkip {
	if c.Trigger != "schedule" || c.ScheduledFor == nil || c.TriggerIndex == nil || c.Attempt != 1 {
		return nil
	}
	i := *c.TriggerIndex
	if i < 0 || i >= len(a.Triggers) || a.Triggers[i].IsWebhook() {
		// The trigger changed since the fire; its grace is unknown, so the run goes ahead.
		return nil
	}
	loc, err := time.LoadLocation(a.Timezone)
	if err != nil {
		loc = time.UTC
	}
	grace := spec.MissedGraceFor(a.Triggers[i], loc, *c.ScheduledFor)
	late := now.Sub(*c.ScheduledFor)
	if late <= grace {
		return nil
	}
	return &gateSkip{gate: store.GateMissed, summary: fmt.Sprintf("the fire at %s was claimed %s late, past its grace of %s",
		c.ScheduledFor.In(loc).Format("2006-01-02 15:04 MST"), late.Round(time.Second), grace)}
}
