package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/build"
	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/metrics"
	"github.com/kensac/orbit/internal/notify"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// buildCredential is the one secret a build sandbox receives: what the CLI
// needs to reach its own backend (sandbox.AllowedEnv).
const buildCredential = "CLAUDE_CODE_OAUTH_TOKEN" //nolint:gosec // G101: the variable's name, not a value

// errDraining stops a build attempt from starting its sandbox on an
// instance that is leaving: the run is handed back to the ready instance.
var errDraining = errors.New("this instance is draining")

// executeBuild runs one build run: the job is resolved here from the
// ticket, its approved plan and configuration, the build runner does the
// work, and the outcome moves the ticket in the same transaction as the
// run (FinishBuild, FailBuild). Like every attempt it is fenced by its
// claim: one that lost it writes nothing more, and one stopped by a drain
// is handed back, its sandbox destroyed on the way out.
func (e *Engine) executeBuild(ctx, actx context.Context, snap *Snapshot, a *spec.Agent, c *store.ClaimedRun, start time.Time, log *slog.Logger) error {
	res, err := e.build(actx, snap, a, c, log)
	if errors.Is(context.Cause(actx), errLostClaim) {
		log.Warn("build attempt stopped: it lost its claim on the run", "attempt", c.Attempt)
		return nil
	}
	var cost *float64
	if res != nil {
		cost = res.CostUSD
	}
	if ctx.Err() != nil || errors.Is(err, build.ErrStopped) {
		log.Info("build handed back", "attempt", c.Attempt, "err", err)
		return e.Store.HandBack(context.WithoutCancel(ctx), c.ID, c.Attempt, cost, nil)
	}
	kind := "claude-code"
	if err != nil {
		rr := store.RunResult{Attempt: c.Attempt, Error: err.Error(), CostUSD: cost}
		if ferr := e.Store.FailBuild(context.WithoutCancel(ctx), c.ID, rr); ferr != nil {
			return ferr
		}
		metrics.RunFinished(c.Agent, "failed", kind, time.Since(start), cost)
		e.deliver(ctx, snap, a, &c.ID, nil, "failure", notify.Message{
			Title: a.Name + " build failed", Body: err.Error(), Priority: notify.High,
			Tags: []string{"orbit", "warning"}, Link: e.runLink(snap, c.ID),
		})
		return nil
	}
	report := buildReport(res)
	out, _ := json.Marshal(res.Output)
	rr := store.RunResult{Attempt: c.Attempt, Summary: res.Output.Summary, Report: report, Output: out,
		CostUSD: res.CostUSD, Usage: res.Usage, Gate: res.GateName}
	pr := res.PRNumber
	if err := e.Store.FinishBuild(context.WithoutCancel(ctx), c.ID, rr, store.TicketUpdate{
		BaseSHA: res.BaseSHA, Branch: res.Branch, PRNumber: &pr, HeadSHA: res.HeadSHA,
	}); err != nil {
		return err
	}
	status := store.StatusSucceeded
	if res.GateName != "" {
		status = store.StatusSkipped
		metrics.RunSkipped(c.Agent, res.GateName)
	}
	metrics.RunFinished(c.Agent, status, kind, time.Since(start), res.CostUSD)
	e.deliver(ctx, snap, a, &c.ID, nil, "report", notify.Message{
		Title: fmt.Sprintf("%s: PR #%d opened", a.Name, res.PRNumber), Body: report, Priority: notify.Default,
		Tags: []string{"orbit", a.Name}, Link: res.PRURL,
	})
	return nil
}

// build resolves the job and runs it.
func (e *Engine) build(ctx context.Context, snap *Snapshot, a *spec.Agent, c *store.ClaimedRun, log *slog.Logger) (*build.Result, error) {
	if e.Builder == nil {
		return nil, errors.New("this orbit process has no build runner")
	}
	d := snap.Config.Deployment
	if d.Build == nil {
		return nil, errors.New("orbit.yaml has no build section")
	}
	if a.Name != d.Build.Agent {
		return nil, fmt.Errorf("agent %q is not the build agent (%q)", a.Name, d.Build.Agent)
	}
	b, err := e.Store.BuildOfRun(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	if b == nil {
		return nil, errors.New("run is not a ticket's build")
	}
	t, err := e.Store.GetTicket(ctx, b.TicketID)
	if err != nil {
		return nil, err
	}
	if t.Repo == nil {
		return nil, errors.New("the ticket names no repository")
	}
	policy, ok := d.Builds(*t.Repo)
	if !ok {
		return nil, fmt.Errorf("orbit does not build %s (build.repositories)", *t.Repo)
	}
	rs := d.Runners[a.Runner]
	if err := e.Store.SetRunner(ctx, c.ID, c.Attempt, "claude-code", rs.Model, a.SHA); err != nil {
		return nil, err
	}
	author, err := build.ParseIdentity(d.Build.Author)
	if err != nil {
		return nil, err
	}
	app, err := github.NewApp(d.GitHub, e.Env, e.Client)
	if err != nil {
		return nil, err
	}
	token, _ := e.Env(d.Build.TokenEnv)
	sb, err := sandbox.NewClient(d.Build.Sandboxd, token)
	if err != nil {
		return nil, fmt.Errorf("sandboxd: %w (%s)", err, d.Build.TokenEnv)
	}
	cred, _ := e.Env(buildCredential)
	ticket, err := e.ticketJSON(ctx, t)
	if err != nil {
		return nil, err
	}

	var review json.RawMessage
	if b.Purpose != store.BuildPurposeFix {
		comments, err := e.Store.BuildComments(ctx, c.ID)
		if err != nil {
			return nil, err
		}
		if len(comments) > 0 {
			review, err = reviewJSON(comments)
			if err != nil {
				return nil, err
			}
		}
	}

	emit := e.events(ctx, c, log, false).emit
	job := build.Job{
		RunID: c.ID, Attempt: c.Attempt, TicketID: t.ID, Title: t.Title, Repo: *t.Repo,
		Plan: b.Plan, PlanVersion: b.PlanVersion, Ticket: ticket, Purpose: b.Purpose, Failure: b.Failure, Review: review,
		Policy: policy, Model: rs.Model, MaxTurns: rs.MaxTurns, Timeout: time.Duration(a.Timeout),
		Credential: cred, Author: author,
	}
	deps := build.Deps{
		App: app, Sandbox: sb, Emit: emit,
		Artifact: func(dir, name, mt string, content []byte) error {
			return e.Store.PutArtifact(ctx, c.ID, c.Attempt, dir, name, mt, content)
		},
		Record: func(r build.PermissionRequest, dec build.Decision) error {
			return e.Store.RecordDecision(context.WithoutCancel(ctx), store.Decision{RunID: c.ID, Attempt: c.Attempt,
				RequestID: r.RequestID, Tool: r.Tool, Input: r.Input, Allowed: dec.Allow, Message: dec.Message})
		},
		Sandboxes: ledger{e.Store, ctx, c},
		Stop: func() error {
			if e.Drain != nil && e.Drain.Draining() {
				return errDraining
			}
			return ctx.Err()
		},
	}
	return e.Builder.Run(ctx, job, deps)
}

// ledger is the store's record of a build run's sandboxes.
type ledger struct {
	s   *store.Store
	ctx context.Context
	c   *store.ClaimedRun
}

func (l ledger) Open() ([]string, error) { return l.s.OpenSandboxes(l.ctx, l.c.ID) }
func (l ledger) Record(id string) error  { return l.s.RecordSandbox(l.ctx, l.c.ID, l.c.Attempt, id) }
func (l ledger) Destroyed(id string) error {
	return l.s.SandboxDestroyed(context.WithoutCancel(l.ctx), id)
}

// buildReport is the build run's report: computed from the result, not
// written by the model, apart from its notes.
func buildReport(r *build.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Opened [PR #%d](%s) on `%s` at `%s`.\n", r.PRNumber, r.PRURL, r.Branch, short(r.HeadSHA))
	if r.Gate != "" {
		fmt.Fprintf(&b, "\nThe model was not called again: %s.\n", r.Gate)
	}
	if r.Diff != nil {
		fmt.Fprintf(&b, "\n%d files, +%d -%d:\n\n", len(r.Diff.Files), r.Diff.Added, r.Diff.Deleted)
		for _, f := range r.Diff.Files {
			fmt.Fprintf(&b, "- %s `%s`\n", f.Status, f.Path)
		}
	}
	if len(r.TrustBoundary) > 0 {
		fmt.Fprintf(&b, "\n**Trust boundary:** touches `%s`.\n", strings.Join(r.TrustBoundary, "`, `"))
	}
	if n := strings.TrimSpace(r.Output.Notes); n != "" {
		b.WriteString("\n**Notes from the build:**\n\n" + n + "\n")
	}
	return b.String()
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// reviewJSON is the review comments a build is given, as the person wrote
// them: what to change and where, nothing about their bookkeeping.
func reviewJSON(comments []store.ReviewComment) (json.RawMessage, error) {
	type comment struct {
		Path      string  `json:"path"`
		StartLine int     `json:"start_line"`
		EndLine   int     `json:"end_line"`
		Body      string  `json:"body"`
		Author    *string `json:"author,omitempty"`
		HeadSHA   string  `json:"head_sha"`
	}
	out := make([]comment, 0, len(comments))
	for _, c := range comments {
		out = append(out, comment{c.Path, c.StartLine, c.EndLine, c.Body, c.Author, c.HeadSHA})
	}
	return json.MarshalIndent(out, "", "  ")
}
