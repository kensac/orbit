package engine

import (
	"context"
	"errors"
	"math"
	"os"
	"time"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// maxSessionDepth bounds the runs one Claude Code session carries. Every
// continued run re-reads all earlier runs' transcripts, tool results
// included, so the context and each run's input tokens grow with the
// chain; past this a fresh session with the thread in its prompt is the
// cheaper, cleaner reset than waiting for the CLI to compact.
const maxSessionDepth = 20

// planSession decides whether this run continues the agent's session. It
// continues only the session of the agent's latest finished run, when that
// run succeeded on Claude Code under the same spec within the resume
// window, no live turn on it failed, the chain is under maxSessionDepth,
// and its transcript is still on disk. Anything else starts fresh. The
// decision and its reason are an event of the run.
func (e *Engine) planSession(ctx context.Context, a *spec.Agent, c *store.ClaimedRun, workdir, home string, emit func(string, any)) (*runner.Session, bool, error) {
	idle := time.Duration(a.Session.Idle)
	prior, err := e.Store.LatestSession(ctx, a.Name, c.TicketID)
	if err != nil {
		return nil, false, err
	}
	reason := ""
	switch {
	case prior == nil:
		reason = "no earlier run"
	case prior.Status != "succeeded":
		reason = "the latest run did not succeed"
	case prior.SessionID == "" || prior.RunnerKind != "claude-code":
		reason = "the latest run left no Claude Code session"
	case prior.SpecSHA != a.SHA:
		reason = "the agent's spec changed since its latest run"
	case time.Since(prior.FinishedAt) > idle:
		reason = "the latest run ended more than " + idle.String() + " ago"
	case prior.Tainted:
		reason = "a live turn on the session failed"
	case prior.Depth >= maxSessionDepth:
		reason = "the session holds the most runs it may"
	default:
		path, ok := runner.SessionFile(home, workdir, prior.SessionID)
		if ok {
			if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
				reason = "the session's transcript is gone (is " + e.DataDir + " a persistent volume?)"
			} else if err != nil {
				return nil, false, err
			}
		}
	}
	sess := &runner.Session{
		Home:     home,
		KeepDays: int(math.Ceil(idle.Hours()/24)) + 1,
		Key:      sessionKey(a.Name, c.TicketID),
	}
	depth := 1
	if reason == "" {
		sess.Resume = prior.SessionID
		if prior.CostUSD != nil {
			sess.BaseCostUSD = *prior.CostUSD
		}
		depth = prior.Depth + 1
	}
	if live := time.Duration(a.Session.Live); live > 0 && e.Live != nil {
		// A live process opens for a person's message: they are likely to
		// write again soon. Any run may use one that is already open.
		sess.Live, sess.LiveIdle, sess.Open = e.Live, live, c.Trigger == "message"
	}
	sess.Started = func(mode, parent string) error {
		emit("orbit.session", map[string]any{"mode": mode, "parent": parent, "depth": depth, "fresh_because": reason})
		return e.Store.SetSession(ctx, c.ID, mode, parent, depth)
	}
	return sess, reason == "", nil
}

// sessionKey names one thread's live process: the agent's general thread, or
// one of its tickets, so a ticket's turn never lands in another thread's
// process.
func sessionKey(agent string, ticketID *string) string {
	if ticketID == nil {
		return agent
	}
	return agent + "/ticket/" + *ticketID
}
