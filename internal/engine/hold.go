package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/metrics"
	"github.com/kensac/orbit/internal/notify"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// A backend that refuses orbit (its login is gone, or the subscription window is spent) would fail, and notify, every unattended run until a person notices. Instead the first such failure holds the runner (store.OpenHold): every unattended run of an agent on it waits queued (store's holdFilter), one notification says why, and the hold closes when a probe succeeds or a person's own run does. /health lists it.

// backendErr marks an error a runner returned, so a hold is decided only on what the backend said, never on a source or a transform that failed with a 401 of its own.
type backendErr struct{ error }

func (b backendErr) Unwrap() error { return b.error }

// authFailures are how a backend says it does not accept orbit's credential, lower case: Claude Code's "Not logged in · Please run /login" and "Invalid API key", an expired OAuth token, and the API's 401 authentication_error (the anthropic runner reports "anthropic 401: ...").
var authFailures = []string{"not logged in", "please run /login", "invalid api key", "oauth token has expired",
	"failed to authenticate", "authentication_error", "authentication_failed", "anthropic 401"}

// Probe timing. HoldProbeEvery is how often a held runner is tried again: often enough that a fixed login resumes the schedule within minutes, rarely enough that a long outage costs a handful of tiny calls an hour. holdProbeTimeout bounds one probe.
const (
	HoldProbeEvery   = 15 * time.Minute
	holdProbeTimeout = 2 * time.Minute
)

// holdFor decides whether a failed run's error means its runner should be held, and returns the hold, or nil.
func (e *Engine) holdFor(ctx context.Context, a *spec.Agent, c *store.ClaimedRun, runErr error) *store.NewHold {
	var be backendErr
	if a == nil || !errors.As(runErr, &be) {
		return nil
	}
	now := time.Now()
	h := &store.NewHold{Runner: a.Runner, RunID: c.ID, FirstProbe: now.Add(HoldProbeEvery)}
	msg := strings.ToLower(be.Error())
	for _, f := range authFailures {
		if strings.Contains(msg, f) {
			h.Kind, h.Reason = store.HoldAuth, firstLine(be.Error())
			return h
		}
	}
	// A rejected window: Claude Code reports it in a rate_limit_event before the turn fails.
	evs, err := e.Store.AttemptRateLimitEvents(ctx, c.ID, c.Attempt)
	if err != nil || len(evs) == 0 {
		return nil
	}
	obs := make([]runner.RateLimitObservation, len(evs))
	for i, ev := range evs {
		obs[i] = runner.RateLimitObservation{RunID: ev.RunID, At: ev.At, Payload: ev.Payload}
	}
	windows, _ := runner.RateLimits(obs, now)
	for _, w := range windows {
		if w.Status != "rejected" || w.Expired {
			continue
		}
		h.Kind, h.Reason = store.HoldRateLimit, fmt.Sprintf("the %s window is spent: %s", w.Window, firstLine(be.Error()))
		if w.ResetsAt != nil && (h.ResetsAt == nil || w.ResetsAt.After(*h.ResetsAt)) {
			// Probing before the window resets would only be refused again.
			h.ResetsAt, h.FirstProbe = w.ResetsAt, *w.ResetsAt
		}
	}
	if h.Kind == "" {
		return nil
	}
	return h
}

// failHeld fails a run whose backend refused orbit and holds its runner. A run a person started (a message, a mail, a manual run) reports its failure as any failure does (fail: in the chat, a reply to the mail, a notification), since that person is waiting on it; the hold opens silently beside it. Otherwise only the run that opens the hold notifies, once, saying why and what resumes it, and a run that finds the runner held already notifies nothing.
func (e *Engine) failHeld(ctx context.Context, snap *Snapshot, a *spec.Agent, c *store.ClaimedRun, start time.Time, runErr error, r *runner.Result, h *store.NewHold) error {
	e.Log.Warn("run failed; holding its runner", "run", c.ID, "agent", c.Agent, "runner", h.Runner, "kind", h.Kind, "err", runErr)
	if personal(c.Trigger) {
		if _, err := e.Store.OpenHold(context.WithoutCancel(ctx), *h); err != nil {
			e.Log.Error("open hold", "runner", h.Runner, "err", err)
		}
		return e.fail(ctx, snap, a, c, start, runErr, r)
	}
	rr := store.RunResult{Attempt: c.Attempt, Status: "failed", Error: runErr.Error()}
	fillResult(&rr, r)
	if err := e.Store.FinishRun(context.WithoutCancel(ctx), c.ID, rr); err != nil {
		return err
	}
	metrics.RunFinished(c.Agent, "failed", runnerKind(snap, a), time.Since(start), rr.CostUSD)
	opened, err := e.Store.OpenHold(context.WithoutCancel(ctx), *h)
	if err != nil {
		e.Log.Error("open hold", "runner", h.Runner, "err", err)
		return nil
	}
	if !opened {
		return nil
	}
	resume := fmt.Sprintf("orbit tries it again every %s", HoldProbeEvery)
	if h.ResetsAt != nil {
		resume = "orbit tries it again once the window resets, at " + h.ResetsAt.UTC().Format(time.RFC3339)
	}
	e.deliver(ctx, snap, a, &c.ID, nil, "hold", notify.Message{
		Title: "orbit: runner " + h.Runner + " is held",
		Body: fmt.Sprintf("%s\n\nScheduled and other unattended runs of every agent on %s wait in the queue instead of failing. %s, and resumes when that succeeds or when you run something yourself.",
			h.Reason, h.Runner, resume),
		Priority: notify.High, Tags: []string{"orbit", "warning"}, Link: snap.Config.Deployment.RunURL(c.ID),
	})
	return nil
}

// personResumed closes the runner's hold when a person's own run on it succeeded: the backend answers again.
func (e *Engine) personResumed(ctx context.Context, a *spec.Agent, c *store.ClaimedRun) {
	if !personal(c.Trigger) {
		return
	}
	closed, err := e.Store.CloseHold(ctx, a.Runner, "run:"+c.ID)
	if err != nil {
		e.Log.Warn("close hold", "runner", a.Runner, "err", err)
		return
	}
	if closed {
		e.Log.Info("runner resumed by a person's run", "runner", a.Runner, "run", c.ID)
		e.wake()
	}
}

// personal reports whether a person started a run of this trigger and is waiting on it; such runs are never held (store's holdFilter).
func personal(trigger string) bool { return trigger == "message" || trigger == "manual" }

// probeOutput is all a probe asks the backend for.
const probeSchema = `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`

// ProbeHolds tries each held runner whose probe is due, once per HoldProbeEvery across instances (store.ClaimProbe), each in its own goroutine so a slow backend holds up nothing. A probe is the smallest call the runner makes: no tools, no sources, one short prompt. It returns how many probes it started.
func (e *Engine) ProbeHolds(ctx context.Context) (int, error) {
	n := 0
	for {
		h, err := e.Store.ClaimProbe(ctx, HoldProbeEvery)
		if err != nil || h == nil {
			return n, err
		}
		n++
		go e.probe(context.WithoutCancel(ctx), h)
	}
}

func (e *Engine) probe(ctx context.Context, h *store.Hold) {
	err := e.tryRunner(ctx, h.Runner)
	if rerr := e.Store.RecordProbe(ctx, h.ID, err); rerr != nil {
		e.Log.Error("record probe", "runner", h.Runner, "err", rerr)
		return
	}
	if err != nil {
		e.Log.Warn("probe failed; the runner stays held", "runner", h.Runner, "err", err)
		return
	}
	e.Log.Info("probe succeeded; the runner resumes", "runner", h.Runner)
	e.wake()
}

// tryRunner makes one minimal call on the runner as configured now.
func (e *Engine) tryRunner(ctx context.Context, name string) error {
	rs, ok := e.Current().Config.Deployment.Runners[name]
	if !ok {
		// The runner left the configuration: nothing of it can be held.
		return nil
	}
	r, err := e.NewRunner(name, rs, e.Env)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(e.DataDir, "probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	pctx, cancel := context.WithTimeout(ctx, holdProbeTimeout)
	defer cancel()
	res, err := r.Run(pctx, runner.Request{RunID: "probe", Workdir: dir, System: "You answer health checks.",
		Prompt: `Reply with {"ok": true}.`, OutputSchema: json.RawMessage(probeSchema)}, func(runner.Event) {})
	if err != nil {
		return err
	}
	if res == nil || len(res.Output) == 0 {
		return errors.New("the probe returned no output")
	}
	return nil
}

func (e *Engine) wake() {
	if e.Wake != nil {
		e.Wake()
	}
}
