package engine

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/status"
	"github.com/kensac/orbit/internal/store"
)

// listenHooks serves the attempt's status socket for the Claude Code hooks
// `orbit hook` posts (internal/status). Each event of the attempt's own
// turn is recorded as an orbit.hook event in the run's stream, and each
// change of the turn's state is stored on the run (hook_status). It
// returns the socket to configure the hooks with, or "" when it cannot
// listen: the run then goes on without hook status, which falls back to
// the run's row, as for any other backend.
func (e *Engine) listenHooks(ctx context.Context, c *store.ClaimedRun, home string, emit func(string, any), log *slog.Logger) (*status.Listener, string) {
	state := harness.StateDir(home)
	sock := status.SocketPath(state)
	t := status.NewTracker(c.Attempt, time.Now())
	l, err := status.Listen(sock, status.SpoolPath(state), func(ev status.Event) {
		if !t.Own(ev) {
			return
		}
		emit("orbit.hook", ev)
		if !t.Apply(ev) {
			return
		}
		b, _ := json.Marshal(t.Snapshot())
		if err := e.Store.SetHookStatus(context.WithoutCancel(ctx), c.ID, c.Attempt, b); err != nil && !errors.Is(err, store.ErrConflict) {
			log.Warn("store hook status", "err", err)
		}
	})
	if err != nil {
		log.Warn("hook status is off for this run: cannot listen", "socket", sock, "err", err)
		return nil, ""
	}
	return l, sock
}
