package tickets

import (
	"context"
	"errors"
	"fmt"

	"github.com/kensac/orbit/internal/build"
	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/store"
)

// stackReady reports whether a stacked ticket may start its build: the
// ticket below it has a PR to build on (review, merging), or no longer
// needs one (merged, done, abandoned: the build starts from the default
// branch). Anything else waits.
func (k *tick) stackReady(ctx context.Context, t *store.Ticket) (bool, error) {
	if t.StackOn == nil {
		return true, nil
	}
	p, err := k.l.Store.GetTicket(ctx, *t.StackOn)
	if err != nil {
		return false, err
	}
	switch p.Status {
	case store.TicketReview, store.TicketMerging, store.TicketMerged, store.TicketDone, store.TicketAbandoned:
		return true, nil
	}
	return false, nil
}

// stacked keeps a stacked ticket in step with the ticket below it, in
// review. While that ticket's PR is open, this one's merge is held, and a
// new head there (a fix, a revise) is restacked onto; once it merged or
// was abandoned, this branch is restacked onto the default branch and the
// PR retargeted, and then it merges like any other. moved means the branch
// changed this tick: the checks are read again next tick.
func (k *tick) stacked(ctx context.Context, t *store.Ticket, pr *github.PullRequest) (hold, moved bool, err error) {
	if t.StackOn == nil {
		return false, false, nil
	}
	p, err := k.l.Store.GetTicket(ctx, *t.StackOn)
	if err != nil {
		return true, false, err
	}
	def := pr.Base.Repo.DefaultBranch
	switch p.Status {
	case store.TicketMerged, store.TicketDone, store.TicketAbandoned:
		if pr.Base.Ref == def {
			return false, false, nil
		}
		return false, true, k.restack(ctx, t, pr, "", fmt.Sprintf("the ticket below it is %s", p.Status))
	case store.TicketReview, store.TicketMerging:
		if p.HeadSHA != nil && p.Branch != nil && (t.BaseSHA == nil || *t.BaseSHA != *p.HeadSHA) {
			return true, true, k.restack(ctx, t, pr, *p.Branch, "the ticket below it moved to "+short(*p.HeadSHA))
		}
	}
	return true, false, nil
}

// restack replays the ticket's own commits onto onto (empty: the default
// branch) by code, records the new base and head, and retargets the PR when
// its base branch changed. A conflict fails the ticket: retrying rebuilds
// it from the plan on the new base.
func (k *tick) restack(ctx context.Context, t *store.Ticket, pr *github.PullRequest, onto, why string) error {
	if k.l.Restack == nil {
		return errors.New("a stacked ticket needs restacking, and the loop has no Restack")
	}
	if t.BaseSHA == nil || t.HeadSHA == nil {
		return k.fail(ctx, t, "a stacked ticket in review with no recorded base or head")
	}
	author, err := build.ParseIdentity(k.cfg.Deployment.Build.Author)
	if err != nil {
		return err
	}
	res, err := k.l.Restack(ctx, k.app, build.Restack{Repo: *t.Repo, TicketID: t.ID, Onto: onto, From: *t.BaseSHA, Head: *t.HeadSHA, Committer: author})
	if errors.Is(err, build.ErrRestackConflict) {
		return k.fail(ctx, t, err.Error()+"; retry rebuilds it on the new base")
	}
	if err != nil {
		return err
	}
	if err := k.l.Store.Restacked(ctx, t.ID, *t.HeadSHA, res.Base, res.Head, res.Branch); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil
		}
		return err
	}
	k.l.Log.Info("ticket restacked", "ticket", t.ID, "onto", res.Branch, "head", short(res.Head), "why", why)
	if pr.Base.Ref == res.Branch {
		return nil
	}
	c, err := k.client(ctx, *t.Repo, github.Deliver, "deliver")
	if err != nil {
		return err
	}
	return c.SetPRBase(ctx, *t.Repo, pr.Number, res.Branch)
}
