package tickets

import (
	"context"
	"errors"
	"time"

	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/usage"
)

// startRace enqueues every lane of an approved race, unless the
// subscription's usage is past the deferral threshold: a race spends two
// or three builds on one plan, so it waits in approved until every window
// drops (docs/tickets.md, Races). ErrBusy waits too: another ticket of the
// repository is building.
func (k *tick) startRace(ctx context.Context, t *store.Ticket) error {
	w, err := usage.Crossed(ctx, k.l.Store, time.Now())
	if err != nil {
		return err
	}
	if w != nil {
		k.l.Log.Info("ticket race waits", "ticket", t.ID, "why", w.Above())
		return nil
	}
	a, err := k.buildAgent()
	if err != nil {
		return err
	}
	runs, err := k.l.Store.StartRace(ctx, store.StartBuild{TicketID: t.ID, Agent: a.Name, SpecSHA: a.SHA, Purpose: store.BuildPurposeBuild})
	if err != nil {
		if errors.Is(err, store.ErrBusy) {
			return nil
		}
		return err
	}
	k.l.Log.Info("ticket race queued", "ticket", t.ID, "runs", runs)
	if k.l.Wake != nil {
		k.l.Wake()
	}
	return nil
}

// raceReview reads each delivered lane's PR and its required checks and
// records them for the comparison a person picks from. Nothing is merged,
// rerun or fixed before the pick.
func (k *tick) raceReview(ctx context.Context, t *store.Ticket) error {
	lanes, err := k.l.Store.RaceLanes(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, l := range lanes {
		if !l.Delivered || l.PRClosedAt != nil {
			continue
		}
		c, err := k.client(ctx, *t.Repo, github.WatchPermissions, "watch")
		if err != nil {
			return err
		}
		pr, err := c.PR(ctx, *t.Repo, *l.PRNumber)
		if err != nil {
			return err
		}
		state, checks := pr.State, ""
		if pr.Merged {
			state = "merged"
		}
		if state == "open" {
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
			switch cs, _ := github.Classify(required, runs, statuses); cs {
			case github.ChecksGreen:
				checks = "green"
			case github.ChecksFailed:
				checks = "failed"
			default:
				checks = "pending"
			}
		}
		if err := k.l.Store.RecordLaneChecks(ctx, t.ID, l.Lane, pr.Head.SHA, checks, state); err != nil {
			return err
		}
	}
	return nil
}

// closeLosers closes the PRs of a picked race's other lanes.
func (k *tick) closeLosers(ctx context.Context, t *store.Ticket) error {
	if t.Race == nil || t.PickedLane == nil {
		return nil
	}
	return k.closeLanes(ctx, t, *t.PickedLane)
}

// closeLanes closes every open lane PR but keep's, with the delivery token,
// and keeps the branches: never deleted, so a person can still read what
// each lane built.
func (k *tick) closeLanes(ctx context.Context, t *store.Ticket, keep string) error {
	lanes, err := k.l.Store.RaceLanes(ctx, t.ID)
	if err != nil {
		return err
	}
	for _, l := range lanes {
		if l.Lane == keep || l.PRNumber == nil || l.PRClosedAt != nil {
			continue
		}
		c, err := k.client(ctx, *t.Repo, github.WatchPermissions, "watch")
		if err != nil {
			return err
		}
		pr, err := c.PR(ctx, *t.Repo, *l.PRNumber)
		if err != nil {
			return err
		}
		state := pr.State
		if pr.Merged {
			state = "merged"
		}
		if state == "open" {
			state = "closed"
			d, err := k.client(ctx, *t.Repo, github.Deliver, "deliver")
			if err != nil {
				return err
			}
			if err := d.ClosePR(ctx, *t.Repo, pr.Number); err != nil {
				return err
			}
			k.l.Log.Info("race lane closed", "ticket", t.ID, "lane", l.Lane, "pr", pr.Number)
		}
		if err := k.l.Store.LaneClosed(ctx, t.ID, l.Lane, state); err != nil {
			return err
		}
	}
	return nil
}
