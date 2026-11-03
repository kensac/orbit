package engine

import (
	"context"
	"time"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// A chat run's permission level (docs/agents/configuration.md, Permissions; docs/api.md, Conversation permissions): the conversation's own version when its person's message started the run, else the agent's. An unattended run never takes a raised conversation level, only a lowered one.

// levelPoll is how often a running chat turn reads its conversation's level: a lowering reaches the process within it, so the next tool call after it is held to the lower level.
const levelPoll = cancelPoll

// levelFor is the level a run gets, and why.
func levelFor(a *spec.Agent, attended bool, cp *store.ConversationPermission, now time.Time) runner.LevelChange {
	ch := runner.LevelChange{Level: a.PermissionLevel(), Choice: runner.LevelChoice{Source: "agent"}}
	if !cp.InForce(now) {
		return ch
	}
	level := a.UnattendedLevel(cp.Level)
	if attended {
		level = a.ResolveLevel(cp.Level)
	}
	if level == ch.Level && !attended {
		return ch
	}
	ch.Level = level
	ch.Choice = runner.LevelChoice{Source: "conversation", Version: cp.Version, SetBy: cp.SetBy, ExpiresAt: cp.ExpiresAt}
	return ch
}

// runLevel reads the conversation's level for a run.
func (e *Engine) runLevel(ctx context.Context, a *spec.Agent, c *store.ClaimedRun) (runner.LevelChange, error) {
	cp, err := e.Store.ConversationPermission(ctx, c.ConversationID)
	if err != nil {
		return runner.LevelChange{}, err
	}
	return levelFor(a, Attended(c.Trigger), cp, time.Now()), nil
}

// watchLevel polls the conversation's level while the turn runs and sends each change, the newest replacing one not yet taken. A failed read keeps the level the turn has.
func (e *Engine) watchLevel(ctx context.Context, a *spec.Agent, c *store.ClaimedRun, cur runner.LevelChange, out chan runner.LevelChange) {
	t := time.NewTicker(levelPoll)
	defer t.Stop()
	attended := Attended(c.Trigger)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cp, err := e.Store.ConversationPermission(ctx, c.ConversationID)
			if err != nil {
				continue
			}
			next := levelFor(a, attended, cp, time.Now())
			if next.Level == cur.Level {
				continue
			}
			cur = next
			select {
			case <-out:
			default:
			}
			out <- next
		}
	}
}
