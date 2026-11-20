// Package boxkeep is the periodic work orbit does on agents' running boxes
// (docs/workspace.md): keeping a GitHub installation token in each one for
// its gh and git, orbit's skills library in its shared profile and the MCP
// servers orbit manages in each of its accounts, and polling each logged-in
// account's usage when that is switched on. It only ever acts through sandboxd's fixed operations, and
// never starts a stopped box.
package boxkeep

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/github"
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

// GHRewrite is how often a box's token is written again; GitHub's
// installation tokens last an hour, and a token written is at most
// ghMintAge old, so a box always holds one with ghMargin left.
const (
	GHRewrite = 45 * time.Minute
	ghMintAge = 10 * time.Minute
	tick      = time.Minute
	tokenLife = time.Hour
)

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
	// GitHub is the deployment's github block, nil without one; App builds
	// the App that mints the box token from it.
	GitHub func() *spec.GitHub
	App    func(*spec.GitHub) (*github.App, error)
	// MCPServers are the MCP servers orbit manages in every account
	// (box.mcp_servers), by name.
	MCPServers func() map[string]json.RawMessage
	// Tick is how often Run looks (default a minute); Now is time.Now.
	Tick time.Duration
	Now  func() time.Time
	Log  *slog.Logger

	tok      *github.Token
	minted   time.Time
	written  map[string]wrote
	cleared  map[string]string
	synced   map[string]string
	nextPoll time.Time
	// round is the running boxes Once found, shared by its steps.
	round *round
}

type round struct {
	c     *sandbox.Client
	boxes []store.Box
	sts   map[string]sandbox.BoxStatus
}

// wrote is the box (its container id) a token was last written to, and when.
type wrote struct {
	id string
	at time.Time
}

func (k *Keeper) now() time.Time {
	if k.Now != nil {
		return k.Now()
	}
	return time.Now()
}

// Run does the work until ctx ends.
func (k *Keeper) Run(ctx context.Context) {
	every := k.Tick
	if every <= 0 {
		every = tick
	}
	if k.PollUsage > 0 {
		k.Log.Info("polling accounts' usage", "every", k.PollUsage)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		k.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once does what is due: the GitHub token of every running box, the skills
// library and managed MCP servers where they changed, and the usage poll
// when its interval has passed.
func (k *Keeper) Once(ctx context.Context) {
	if len(k.Agents()) == 0 {
		return
	}
	c, boxes, sts := k.running(ctx)
	k.round = &round{c: c, boxes: boxes, sts: sts}
	defer func() { k.round = nil }()
	k.GHOnce(ctx)
	k.SyncOnce(ctx)
	if k.PollUsage > 0 && !k.now().Before(k.nextPoll) {
		k.nextPoll = k.now().Add(k.PollUsage)
		k.PollOnce(ctx)
	}
}

// GHOnce writes the box token into each running box that has none from
// this instance, was recreated since, or had it written GHRewrite ago; with
// the token switched off (github.box.token false, or no github block) it
// removes one from each running box once.
func (k *Keeper) GHOnce(ctx context.Context) {
	var g *spec.GitHub
	if k.GitHub != nil {
		g = k.GitHub()
	}
	on := g.BoxTokenOn() && k.App != nil
	if k.written == nil {
		k.written, k.cleared = map[string]wrote{}, map[string]string{}
	}
	c, boxes, sts := k.running(ctx)
	now := k.now()
	for _, b := range boxes {
		id := sts[b.Agent].ID
		if !on {
			if k.cleared[b.Agent] == id {
				continue
			}
			if err := c.ClearGHToken(ctx, b.Agent); err != nil {
				k.Log.Warn("gh token: clear", "agent", b.Agent, "err", err)
				continue
			}
			k.cleared[b.Agent] = id
			delete(k.written, b.Agent)
			continue
		}
		delete(k.cleared, b.Agent)
		if w, ok := k.written[b.Agent]; ok && w.id == id && now.Sub(w.at) < GHRewrite {
			continue
		}
		tok, err := k.token(ctx, g)
		if err != nil {
			k.Log.Error("gh token: mint", "err", err)
			return
		}
		if err := c.WriteGHToken(ctx, b.Agent, tok.Value); err != nil {
			k.Log.Warn("gh token: write", "agent", b.Agent, "err", err)
			continue
		}
		k.written[b.Agent] = wrote{id: id, at: now}
		k.Log.Info("gh token written", "agent", b.Agent, "expires_at", tok.ExpiresAt)
	}
}

// token is a box token at most ghMintAge old, minting one when needed.
// Tokens are left to expire, never revoked early: a box written a minute
// ago still holds the previous one.
func (k *Keeper) token(ctx context.Context, g *spec.GitHub) (*github.Token, error) {
	if k.tok != nil && k.now().Sub(k.minted) < ghMintAge {
		return k.tok, nil
	}
	app, err := k.App(g)
	if err != nil {
		return nil, err
	}
	tok, repos, err := app.BoxToken(ctx, g)
	if err != nil {
		return nil, err
	}
	k.tok, k.minted = tok, k.now()
	k.Log.Info("gh token minted for boxes", "repositories", len(repos))
	return tok, nil
}

// running is each box row whose agent has a box now and whose box is
// running, with sandboxd's client.
func (k *Keeper) running(ctx context.Context) (*sandbox.Client, []store.Box, map[string]sandbox.BoxStatus) {
	if r := k.round; r != nil {
		return r.c, r.boxes, r.sts
	}
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
