// Package boxkeep is the periodic work orbit does on agents' running boxes
// (docs/workspace.md, Accounts): polling each logged-in account's usage when
// that is switched on. It only ever acts through sandboxd's fixed
// operations, and never starts a stopped box.
package boxkeep

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// MinPollUsage is the shortest ORBIT_ACCOUNT_USAGE_POLL: the endpoint is
// undocumented, and the CLI itself asks it rarely.
const MinPollUsage = 5 * time.Minute

// ParsePollUsage reads ORBIT_ACCOUNT_USAGE_POLL: unset, "0" or "off" is off
// (the default); otherwise a Go duration of at least MinPollUsage.
func ParsePollUsage(v string) (time.Duration, error) {
	switch strings.TrimSpace(v) {
	case "", "0", "off":
		return 0, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return 0, fmt.Errorf("ORBIT_ACCOUNT_USAGE_POLL %q: want a duration such as 10m, or off", v)
	}
	if d < MinPollUsage {
		return 0, fmt.Errorf("ORBIT_ACCOUNT_USAGE_POLL %s: at least %s", d, MinPollUsage)
	}
	return d, nil
}

// RecordUsage stores an observation of an account's usage when it is one
// and newer than what is stored, and reports whether it was stored.
func RecordUsage(ctx context.Context, st *store.Store, id string, u sandbox.AccountUsage) (bool, error) {
	if u.ObservedAt == nil {
		return false, nil
	}
	if u.Windows == nil {
		u.Windows = []sandbox.UsageWindow{}
	}
	b, err := json.Marshal(u)
	if err != nil {
		return false, err
	}
	return st.RecordAccountUsage(ctx, id, b, *u.ObservedAt)
}

// BoxAgents are the agents of cfg that have a box: those whose chats run in
// a workstation sandbox (docs/api.md, Terminals).
func BoxAgents(cfg *spec.Config) []string {
	var out []string
	for name, a := range cfg.Agents {
		if a.Sandbox == spec.SandboxWorkstation {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Keeper runs the periodic box work.
type Keeper struct {
	Store *store.Store
	// Boxes is sandboxd's client; an error skips a round.
	Boxes func() (*sandbox.Client, error)
	// Agents are the agents that have a box now (their chats run in a
	// workstation sandbox); a box row of any other agent is left alone.
	Agents func() []string
	// PollUsage is how often each logged-in account's usage is polled from
	// /api/oauth/usage; 0 is never.
	PollUsage time.Duration
	Log       *slog.Logger
}

// Run does the work until ctx ends. With nothing switched on it returns at
// once.
func (k *Keeper) Run(ctx context.Context) {
	if k.PollUsage <= 0 {
		return
	}
	k.Log.Info("polling accounts' usage", "every", k.PollUsage)
	t := time.NewTicker(k.PollUsage)
	defer t.Stop()
	for {
		k.PollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// running is each box row whose agent has a box now and whose box is
// running, with sandboxd's client.
func (k *Keeper) running(ctx context.Context) (*sandbox.Client, []store.Box, map[string]sandbox.BoxStatus) {
	c, err := k.Boxes()
	if err != nil {
		k.Log.Warn("boxes: sandboxd", "err", err)
		return nil, nil, nil
	}
	rows, err := k.Store.Boxes(ctx)
	if err != nil {
		k.Log.Warn("boxes: list", "err", err)
		return nil, nil, nil
	}
	agents := k.Agents()
	var out []store.Box
	sts := map[string]sandbox.BoxStatus{}
	for _, b := range rows {
		if !slices.Contains(agents, b.Agent) {
			continue
		}
		st, err := c.Box(ctx, b.Agent)
		if err != nil {
			k.Log.Warn("boxes: status", "agent", b.Agent, "err", err)
			continue
		}
		if st.State == sandbox.BoxRunning {
			out = append(out, b)
			sts[b.Agent] = st
		}
	}
	return c, out, sts
}

// PollOnce polls every logged-in, mounted account of every running box.
func (k *Keeper) PollOnce(ctx context.Context) {
	c, boxes, sts := k.running(ctx)
	for _, b := range boxes {
		as, err := k.Store.ListAccounts(ctx, b.ID)
		if err != nil {
			k.Log.Warn("usage poll: accounts", "agent", b.Agent, "err", err)
			continue
		}
		for _, a := range as {
			if !a.LoggedIn || !slices.Contains(sts[b.Agent].Accounts, a.Slot) {
				continue
			}
			u, err := c.PollAccountUsage(ctx, b.Agent, a.Slot)
			if err != nil {
				k.Log.Warn("usage poll", "agent", b.Agent, "account", a.ID, "err", err)
				continue
			}
			if _, err := RecordUsage(ctx, k.Store, a.ID, u); err != nil {
				k.Log.Warn("usage poll: record", "account", a.ID, "err", err)
			}
		}
	}
}
