// Package tickets is the ticket loop: the code that carries an approved
// request from its build to done (docs/tickets.md, Lifecycle). Every tick it
// reads each ticket in flight and GitHub's state for it, and makes the one
// guarded transition that state calls for. GitHub is the truth for the
// branch, the PR, its checks and its merge, so a crash between a side
// effect and its row heals on the next tick: the loop holds no state of its
// own between ticks.
package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/build"
	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// TickEvery is how often the loop reconciles. Checks take minutes, so
// thirty seconds adds little to a merge, and a tick's few GitHub reads per
// ticket stay far inside an installation's 5000 requests an hour.
const TickEvery = 30 * time.Second

// MaxFixAttempts bounds the fix runs a PR's failing checks get before the
// ticket fails for a person: each is a full build, and a check that two
// attempts could not fix is one a person should look at.
const MaxFixAttempts = 2

// Failure report bounds: the end of each failing job's log, for at most
// maxFailing checks. A fix needs the failure, not the whole log, and the
// report rides in its prompt.
const (
	logTailBytes = 16 << 10
	maxFailing   = 5
)

// MergeRule is the rule a ticket's merge is proposed with: the build
// agent's rule for github:merge, except that orbit's own repository is
// always approve. A merge to orbit replaces the process that made it, so a
// person approves every one; this is code, not configuration
// (docs/tickets.md, Self-hosting).
func MergeRule(repo string, specRule spec.Rule) spec.Rule {
	if specRule == spec.RuleDeny {
		return spec.RuleDeny
	}
	if repo == spec.SelfRepo {
		return spec.RuleApprove
	}
	return specRule
}

// Loop reconciles tickets.
type Loop struct {
	Store  *store.Store
	Config func() *spec.Config
	Env    func(string) (string, bool)
	Client *http.Client
	// Revision is the commit this process runs (orbit version): a merged
	// orbit ticket is done when it is the merge commit or descends from it.
	Revision string
	Log      *slog.Logger
	// Approval tells the person a merge waits for them, as any action
	// that needs approval does.
	Approval func(ctx context.Context, agent, runID, actionID, title, body string)
	// Wake nudges the executors and the action loop after an enqueue or
	// an auto merge.
	Wake func()
}

// Run ticks until ctx ends.
func (l *Loop) Run(ctx context.Context) {
	t := time.NewTicker(TickEvery)
	defer t.Stop()
	for {
		if _, err := l.Store.WithTicketLoopLock(ctx, func() error { return l.Tick(ctx) }); err != nil && ctx.Err() == nil {
			l.Log.Error("ticket loop", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// tick is one pass's GitHub access: tokens minted on first use per
// repository and permission set, revoked when the pass ends.
type tick struct {
	l      *Loop
	cfg    *spec.Config
	app    *github.App
	tokens map[string]*github.Token
}

func (k *tick) client(ctx context.Context, repo string, perms github.Permissions, name string) (*github.Client, error) {
	key := repo + " " + name
	if t, ok := k.tokens[key]; ok {
		return t.Client(), nil
	}
	t, err := k.app.Token(ctx, repo, perms)
	if err != nil {
		return nil, err
	}
	k.tokens[key] = t
	return t.Client(), nil
}

// Tick reconciles every ticket in flight once. One ticket's error is logged
// and does not stop the others.
func (l *Loop) Tick(ctx context.Context) error {
	cfg := l.Config()
	d := cfg.Deployment
	if d.GitHub == nil || d.Build == nil {
		return nil
	}
	app, err := github.NewApp(d.GitHub, l.Env, l.Client)
	if err != nil {
		return err
	}
	k := &tick{l: l, cfg: cfg, app: app, tokens: map[string]*github.Token{}}
	defer func() {
		for _, t := range k.tokens {
			_ = t.Revoke(context.WithoutCancel(ctx))
		}
	}()
	ts, err := l.Store.TicketsInFlight(ctx)
	if err != nil {
		return err
	}
	for i := range ts {
		t := &ts[i]
		if t.Repo == nil {
			continue
		}
		if _, ok := d.Builds(*t.Repo); !ok {
			continue
		}
		if err := k.reconcile(ctx, t); err != nil && ctx.Err() == nil {
			l.Log.Warn("ticket", "ticket", t.ID, "status", t.Status, "err", err)
		}
	}
	return nil
}

func (k *tick) reconcile(ctx context.Context, t *store.Ticket) error {
	switch t.Status {
	case store.TicketApproved:
		return k.startBuild(ctx, t, store.BuildPurposeBuild, nil)
	case store.TicketBuilding:
		return k.building(ctx, t)
	case store.TicketReview:
		return k.review(ctx, t)
	case store.TicketMerging:
		return k.merging(ctx, t)
	case store.TicketMerged:
		return k.merged(ctx, t)
	case store.TicketFailed:
		return k.failedMerge(ctx, t)
	case store.TicketAbandoned:
		return k.abandoned(ctx, t)
	}
	return nil
}

func (k *tick) buildAgent() (*spec.Agent, error) {
	a, ok := k.cfg.Agents[k.cfg.Deployment.Build.Agent]
	if !ok {
		return nil, fmt.Errorf("build agent %q is not configured", k.cfg.Deployment.Build.Agent)
	}
	return a, nil
}

// startBuild enqueues a build or a fix. ErrBusy waits: another ticket of
// the repository is building.
func (k *tick) startBuild(ctx context.Context, t *store.Ticket, purpose string, failure json.RawMessage) error {
	a, err := k.buildAgent()
	if err != nil {
		return err
	}
	id, err := k.l.Store.StartBuild(ctx, store.StartBuild{TicketID: t.ID, Agent: a.Name, SpecSHA: a.SHA, Purpose: purpose, Failure: failure})
	if errors.Is(err, store.ErrBusy) {
		return nil
	}
	if err != nil {
		return err
	}
	k.l.Log.Info("ticket build queued", "ticket", t.ID, "run", id, "purpose", purpose)
	if k.l.Wake != nil {
		k.l.Wake()
	}
	return nil
}

// building fails a ticket whose build run ended without moving it: the
// reaper failed the run after its attempts ran out, or it was cancelled.
// A run that finished moves its ticket itself (FinishBuild, FailBuild).
func (k *tick) building(ctx context.Context, t *store.Ticket) error {
	b, err := k.l.Store.LatestBuild(ctx, t.ID)
	if err != nil {
		return err
	}
	if b == nil {
		return k.fail(ctx, t, "building with no build run")
	}
	if b.RunStatus == "failed" || b.RunStatus == "cancelled" {
		why := b.RunStatus
		if b.RunError != nil {
			why += ": " + *b.RunError
		}
		return k.fail(ctx, t, "build run "+why)
	}
	return nil
}

func (k *tick) fail(ctx context.Context, t *store.Ticket, why string) error {
	err := k.l.Store.TransitionTicket(ctx, t.ID, store.TicketFailed, "orbit", store.TicketUpdate{Error: why})
	if errors.Is(err, store.ErrConflict) {
		return nil // it moved on meanwhile; the next tick reads it again
	}
	return err
}

func (k *tick) pr(ctx context.Context, t *store.Ticket) (*github.Client, *github.PullRequest, error) {
	if t.PRNumber == nil {
		return nil, nil, errors.New("no PR recorded")
	}
	c, err := k.client(ctx, *t.Repo, github.WatchPermissions, "watch")
	if err != nil {
		return nil, nil, err
	}
	pr, err := c.PR(ctx, *t.Repo, *t.PRNumber)
	return c, pr, err
}

// review waits for the PR's checks at the head orbit built, and then
// proposes the merge, or a fix, or fails the ticket.
func (k *tick) review(ctx context.Context, t *store.Ticket) error {
	c, pr, err := k.pr(ctx, t)
	if github.IsStatus(err, http.StatusNotFound) {
		return k.fail(ctx, t, fmt.Sprintf("PR #%d is gone", *t.PRNumber))
	}
	if err != nil {
		return err
	}
	switch {
	case pr.Merged:
		return k.toMerged(ctx, t, pr.MergeCommitSHA, "merged on GitHub")
	case pr.State != "open":
		return k.fail(ctx, t, fmt.Sprintf("PR #%d was closed without merging", pr.Number))
	case t.HeadSHA == nil || pr.Head.SHA != *t.HeadSHA:
		return k.fail(ctx, t, fmt.Sprintf("PR #%d's head moved to %s after orbit built it; orbit merges only what it built", pr.Number, short(pr.Head.SHA)))
	}
	required, err := c.RequiredChecks(ctx, *t.Repo, pr.Base.Ref)
	if err != nil {
		return err
	}
	runs, err := c.CheckRuns(ctx, *t.Repo, pr.Head.SHA)
	if err != nil {
		return err
	}
	statuses, err := c.Statuses(ctx, *t.Repo, pr.Head.SHA)
	if err != nil {
		return err
	}
	state, failing := github.Classify(required, runs, statuses)
	switch state {
	case github.ChecksGreen:
		return k.proposeMerge(ctx, t, c, pr)
	case github.ChecksFailed:
		return k.checksFailed(ctx, t, failing)
	}
	return nil
}

// proposeMerge stores the github:merge action with its rule and moves the
// ticket to merging. A build agent whose spec denies github:merge leaves
// the ticket in review for a person to merge on GitHub.
func (k *tick) proposeMerge(ctx context.Context, t *store.Ticket, c *github.Client, pr *github.PullRequest) error {
	a, err := k.buildAgent()
	if err != nil {
		return err
	}
	rule := MergeRule(*t.Repo, a.RuleFor(actions.MergeType))
	if rule == spec.RuleDeny {
		return nil
	}
	b, err := k.l.Store.LatestBuild(ctx, t.ID)
	if err != nil || b == nil {
		return fmt.Errorf("the ticket's build run: %v", err)
	}
	params, _ := json.Marshal(map[string]any{"repo": *t.Repo, "pull_number": pr.Number, "head_sha": pr.Head.SHA})
	reason := fmt.Sprintf("Every check the merge gate requires passed on %s (PR #%d, %q).", short(pr.Head.SHA), pr.Number, pr.Title)
	if t.BaseSHA != nil {
		if _, files, err := c.Compare(ctx, *t.Repo, *t.BaseSHA, pr.Head.SHA); err == nil {
			if trust := build.TrustBoundary(*t.Repo, files); len(trust) > 0 {
				reason += fmt.Sprintf(" Trust boundary: this change touches what orbit itself may do: %s.", strings.Join(trust, ", "))
			}
		}
	}
	id, err := k.l.Store.ProposeMerge(ctx, store.MergeProposal{TicketID: t.ID, RunID: b.RunID, Agent: a.Name,
		HeadSHA: pr.Head.SHA, Params: params, Reason: reason, Rule: string(rule)})
	if errors.Is(err, store.ErrBusy) || errors.Is(err, store.ErrConflict) {
		return nil // one merge per repository at a time; this one waits in review
	}
	if err != nil {
		return err
	}
	k.l.Log.Info("ticket merge proposed", "ticket", t.ID, "action", id, "rule", rule)
	if rule == spec.RuleApprove && k.l.Approval != nil {
		k.l.Approval(ctx, a.Name, b.RunID, id, fmt.Sprintf("%s wants to merge %s#%d", a.Name, *t.Repo, pr.Number),
			reason+"\n\n```json\n"+string(params)+"\n```")
	}
	if rule == spec.RuleAuto && k.l.Wake != nil {
		k.l.Wake()
	}
	return nil
}

// checksFailed starts a fix with the failure report, or fails the ticket
// once MaxFixAttempts fixes were spent on this PR.
func (k *tick) checksFailed(ctx context.Context, t *store.Ticket, failing []github.Failing) error {
	names := make([]string, 0, len(failing))
	for _, f := range failing {
		s := f.Name + " (" + f.Conclusion + ")"
		if f.URL != "" {
			s += " " + f.URL
		}
		names = append(names, s)
	}
	fixes, err := k.l.Store.FixesSinceBuild(ctx, t.ID)
	if err != nil {
		return err
	}
	if fixes >= MaxFixAttempts {
		return k.fail(ctx, t, fmt.Sprintf("checks failed after %d fix attempts: %s", fixes, strings.Join(names, "; ")))
	}
	report, err := k.failureReport(ctx, *t.Repo, failing)
	if err != nil {
		return err
	}
	return k.startBuild(ctx, t, store.BuildPurposeFix, report)
}

// failureReport is what a fix is told, collected by code: each failing
// check's name, conclusion and URL, and the end of its Actions job log.
func (k *tick) failureReport(ctx context.Context, repo string, failing []github.Failing) (json.RawMessage, error) {
	type entry struct {
		github.Failing
		LogTail string `json:"log_tail,omitempty"`
		LogNote string `json:"log_note,omitempty"`
	}
	var out []entry
	var c *github.Client
	for _, f := range failing[:min(len(failing), maxFailing)] {
		e := entry{Failing: f}
		if f.JobID > 0 {
			if c == nil {
				var err error
				if c, err = k.client(ctx, repo, github.FailureReport, "failures"); err != nil {
					return nil, err
				}
			}
			tail, err := c.JobLogTail(ctx, repo, f.JobID, logTailBytes)
			if err != nil {
				e.LogNote = "orbit could not read the job log: " + err.Error()
			} else {
				e.LogTail = tail
			}
		}
		out = append(out, e)
	}
	return json.Marshal(out)
}

// merging follows the merge action. GitHub decides what happened when the
// action failed: an interrupted merge may have merged.
func (k *tick) merging(ctx context.Context, t *store.Ticket) error {
	if t.MergeActionID == nil {
		return k.fail(ctx, t, "merging with no merge action")
	}
	act, err := k.l.Store.GetAction(ctx, *t.MergeActionID)
	if err != nil {
		return err
	}
	switch act.Status {
	case "executed":
		var r struct {
			MergeSHA string `json:"merge_sha"`
		}
		_ = json.Unmarshal(act.Result, &r)
		return k.toMerged(ctx, t, r.MergeSHA, "merged by orbit")
	case "failed", "rejected", "denied":
		_, pr, err := k.pr(ctx, t)
		if err != nil {
			return err
		}
		if pr.Merged {
			return k.toMerged(ctx, t, pr.MergeCommitSHA, "the merge happened; its action did not record it")
		}
		why := "merge " + act.Status
		if act.Error != nil {
			why += ": " + *act.Error
		}
		if act.DecidedBy != nil && act.Status == "rejected" {
			why += " by " + *act.DecidedBy
		}
		return k.fail(ctx, t, why)
	}
	return nil
}

// failedMerge heals a ticket failed while merging whose PR did merge.
func (k *tick) failedMerge(ctx context.Context, t *store.Ticket) error {
	if t.PRNumber == nil {
		return nil
	}
	_, pr, err := k.pr(ctx, t)
	if err != nil || !pr.Merged {
		return err
	}
	return k.toMerged(ctx, t, pr.MergeCommitSHA, "the PR merged after the ticket failed")
}

func (k *tick) toMerged(ctx context.Context, t *store.Ticket, sha, reason string) error {
	err := k.l.Store.TransitionTicket(ctx, t.ID, store.TicketMerged, "orbit", store.TicketUpdate{MergeSHA: sha, Reason: reason})
	if errors.Is(err, store.ErrConflict) {
		return nil
	}
	if err != nil {
		return err
	}
	t.Status, t.MergeSHA = store.TicketMerged, &sha
	return k.merged(ctx, t)
}

// merged is done at once for a repository whose CD deploys on merge, and
// for orbit itself once this process runs the merge commit or a descendant
// of it: a merge that never deploys stays visible in merged.
func (k *tick) merged(ctx context.Context, t *store.Ticket) error {
	done := func(why string) error {
		err := k.l.Store.TransitionTicket(ctx, t.ID, store.TicketDone, "orbit", store.TicketUpdate{Reason: why})
		if errors.Is(err, store.ErrConflict) {
			return nil
		}
		return err
	}
	if *t.Repo != spec.SelfRepo {
		return done("merged; the repository deploys on merge")
	}
	if t.MergeSHA == nil || *t.MergeSHA == "" || !isSHA(k.l.Revision) {
		return nil
	}
	if k.l.Revision == *t.MergeSHA {
		return done("orbit is running the merge commit")
	}
	c, err := k.client(ctx, *t.Repo, github.WatchPermissions, "watch")
	if err != nil {
		return err
	}
	status, _, err := c.Compare(ctx, *t.Repo, *t.MergeSHA, k.l.Revision)
	if err != nil {
		return err
	}
	if status == "identical" || status == "ahead" {
		return done("orbit is running " + short(k.l.Revision) + ", which includes the merge commit")
	}
	return nil
}

// abandoned closes the PR of a ticket a person abandoned; the branch stays.
func (k *tick) abandoned(ctx context.Context, t *store.Ticket) error {
	_, pr, err := k.pr(ctx, t)
	if err != nil || pr.State != "open" {
		return err
	}
	c, err := k.client(ctx, *t.Repo, github.Deliver, "deliver")
	if err != nil {
		return err
	}
	return c.ClosePR(ctx, *t.Repo, pr.Number)
}

func isSHA(s string) bool {
	return len(s) == 40 && strings.Trim(s, "0123456789abcdef") == ""
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
