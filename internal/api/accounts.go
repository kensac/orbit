package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/kensac/orbit/internal/boxkeep"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/store"
)

// Claude accounts in an agent's box (docs/workspace.md, Accounts; docs/api.md,
// Accounts). An account is a directory on its own volume in the box, used as
// CLAUDE_CONFIG_DIR; its login (credentials whose refresh token rotates)
// lives only there. orbit keeps what a fixed script reads of it: email,
// organization, subscription, expiries. Never a token.

// accountRefreshTimeout bounds reading every account of a box on a list.
const accountRefreshTimeout = 20 * time.Second

// accountJSON is an account as the API shows it: the row and its directory.
type accountJSON struct {
	store.BoxAccount
	Dir string `json:"dir"`
}

func showAccount(a store.BoxAccount) accountJSON {
	return accountJSON{BoxAccount: a, Dir: sandbox.AccountDir(a.Slot)}
}

// workspaceAccount resolves an account id within a box workspace.
func (s *Server) workspaceAccount(w http.ResponseWriter, r *http.Request, b store.Box, id string) (store.BoxAccount, bool) {
	a, err := s.Store.GetAccount(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && a.WorkspaceID != b.ID) {
		writeCheck(w, http.StatusNotFound, "not_found", "no such account in this workspace")
		return a, false
	}
	if err != nil {
		respond(w, nil, err)
		return a, false
	}
	return a, true
}

// epochMillis is a CLI time (milliseconds since the epoch), nil when absent.
func epochMillis(ms int64) *time.Time {
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}

// refreshAccount reads an account's login and the usage its status line
// recorded from the running box and stores them, returning the account as
// stored (unchanged where a read failed).
func (s *Server) refreshAccount(ctx context.Context, c *sandbox.Client, a store.BoxAccount) store.BoxAccount {
	if u, err := c.AccountUsage(ctx, a.Agent, a.Slot); err != nil {
		s.Log.Warn("account: read its usage", "account", a.ID, "agent", a.Agent, "err", err)
	} else if _, err := boxkeep.RecordUsage(ctx, s.Store, a.ID, u); err != nil {
		s.Log.Warn("account: record its usage", "account", a.ID, "err", err)
	}
	st, err := c.AccountStatus(ctx, a.Agent, a.Slot)
	if err != nil {
		s.Log.Warn("account: read its login", "account", a.ID, "agent", a.Agent, "err", err)
		if got, gerr := s.Store.GetAccount(ctx, a.ID); gerr == nil {
			return got
		}
		return a
	}
	got, err := s.Store.RecordAccountLogin(ctx, a.ID, store.AccountLogin{
		LoggedIn: st.LoggedIn, Email: st.Email, AccountUUID: st.AccountUUID, OrgID: st.OrgID, OrgName: st.OrgName,
		SubscriptionType: st.SubscriptionType, RateLimitTier: st.RateLimitTier,
		AccessExpiresAt: epochMillis(st.AccessExpiresAt), RefreshExpiresAt: epochMillis(st.RefreshExpiresAt),
	})
	if err != nil {
		s.Log.Warn("account: record its login", "account", a.ID, "err", err)
		return a
	}
	return got
}

// refreshed reads accounts from the box when it is running; a stopped box
// is not started for a read, and its accounts are returned as last read.
func (s *Server) refreshed(ctx context.Context, b store.Box, as []store.BoxAccount) []store.BoxAccount {
	if len(as) == 0 || s.Boxes == nil {
		return as
	}
	c, err := s.Boxes()
	if err != nil {
		return as
	}
	ctx, cancel := context.WithTimeout(ctx, accountRefreshTimeout)
	defer cancel()
	st, err := c.Box(ctx, b.Agent)
	if err != nil || st.State != sandbox.BoxRunning {
		return as
	}
	out := make([]store.BoxAccount, len(as))
	for i, a := range as {
		out[i] = a
		if slices.Contains(st.Accounts, a.Slot) {
			out[i] = s.refreshAccount(ctx, c, a)
		}
	}
	return out
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	b, ok := s.workspaceBox(w, r)
	if !ok {
		return
	}
	as, err := s.Store.ListAccounts(r.Context(), b.ID)
	if err != nil {
		respond(w, nil, err)
		return
	}
	as = s.refreshed(r.Context(), b, as)
	out := make([]accountJSON, 0, len(as))
	for _, a := range as {
		out = append(out, showAccount(a))
	}
	respond(w, map[string]any{"accounts": out}, nil)
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	b, ok := s.workspaceBox(w, r)
	if !ok {
		return
	}
	a, ok := s.workspaceAccount(w, r, b, r.PathValue("aid"))
	if !ok {
		return
	}
	respond(w, map[string]any{"account": showAccount(s.refreshed(r.Context(), b, []store.BoxAccount{a})[0])}, nil)
}

func (s *Server) renameAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaceGroup(w, r) {
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if !decode(w, r, 1<<10, &body, bodyRequired) {
		return
	}
	b, ok := s.workspaceBox(w, r)
	if !ok {
		return
	}
	if !store.ValidAccountLabel(body.Label) {
		writeErr(w, http.StatusBadRequest, "label: 1 to 100 bytes")
		return
	}
	a, ok := s.workspaceAccount(w, r, b, r.PathValue("aid"))
	if !ok {
		return
	}
	a, err := s.Store.RenameAccount(r.Context(), a.ID, body.Label)
	if errors.Is(err, store.ErrConflict) {
		writeCheck(w, http.StatusConflict, "exists", err.Error())
		return
	}
	respond(w, map[string]any{"account": showAccount(a)}, err)
}

// loginBody is a login terminal's size.
type loginBody struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

func (l loginBody) valid(w http.ResponseWriter) bool {
	if (sandbox.Resize{Cols: l.Cols, Rows: l.Rows}).Validate() != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("cols and rows: want 1 to %d and 1 to %d", sandbox.MaxCols, sandbox.MaxRows))
		return false
	}
	return true
}

func loginTerminal(b store.Box, a store.BoxAccount, l loginBody, by string) store.NewBoxTerminal {
	return store.NewBoxTerminal{WorkspaceID: b.ID, Preset: sandbox.TTYLogin, Title: "Log in: " + a.Label,
		Cols: l.Cols, Rows: l.Rows, CreatedBy: by}
}

// createAccount records an account, mounts its volume and opens a terminal
// running `claude auth login` under its directory. A running box gains a
// mount only by being recreated, which ends its terminals: with any running
// that needs restart: true.
func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaceGroup(w, r) {
		return
	}
	var body struct {
		Label   string `json:"label"`
		Cols    int    `json:"cols"`
		Rows    int    `json:"rows"`
		Restart bool   `json:"restart"`
	}
	if !decode(w, r, 1<<10, &body, bodyRequired) {
		return
	}
	b, ok := s.workspaceBox(w, r)
	if !ok {
		return
	}
	size := loginBody{Cols: body.Cols, Rows: body.Rows}
	if !store.ValidAccountLabel(body.Label) {
		writeErr(w, http.StatusBadRequest, "label: 1 to 100 bytes")
		return
	}
	if !size.valid(w) {
		return
	}
	c, ok := s.boxClient(w)
	if !ok {
		return
	}
	ctx := r.Context()
	st, err := c.Box(ctx, b.Agent)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	running := st.State == sandbox.BoxRunning
	if running && !body.Restart {
		n, err := s.Store.RunningTerminals(ctx, b.ID)
		if err != nil {
			respond(w, nil, err)
			return
		}
		if n > 0 {
			writeCheck(w, http.StatusConflict, "busy", fmt.Sprintf("the box restarts to mount a new account, which ends its %d running terminals: send restart: true", n))
			return
		}
	}
	by := actor(r, "operator")
	a, err := s.Store.CreateAccount(ctx, b.ID, body.Label, by)
	switch {
	case errors.Is(err, store.ErrConflict):
		writeCheck(w, http.StatusConflict, "exists", err.Error())
		return
	case errors.Is(err, store.ErrLimit):
		writeCheck(w, http.StatusConflict, "limit", err.Error())
		return
	case err != nil:
		respond(w, nil, err)
		return
	}
	s.Log.Info("account created", "account", a.ID, "slot", a.Slot, "agent", b.Agent, "by", by)
	slots, err := s.Store.AccountSlots(ctx, b.ID)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if running {
		ended, err := s.Store.EndRunningTerminals(ctx, b.ID, store.EndBoxRestarted)
		if err != nil {
			respond(w, nil, err)
			return
		}
		st, err = c.RecreateBox(ctx, b.Agent, slots)
		if err == nil {
			s.Log.Info("box recreated for an account", "agent", b.Agent, "account", a.ID, "terminals_ended", len(ended))
		}
	} else {
		st, err = c.EnsureBox(ctx, b.Agent, slots)
	}
	if err != nil {
		// The account is kept: the box mounts it the next time it starts.
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if !slices.Contains(st.Accounts, a.Slot) {
		writeErr(w, http.StatusBadGateway, "sandboxd did not mount the new account")
		return
	}
	if err := c.PrepareAccount(ctx, b.Agent, a.Slot); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	n := loginTerminal(b, a, size, by)
	n.AccountID = a.ID
	t, err := s.Store.CreateTerminal(ctx, n)
	if errors.Is(err, store.ErrLimit) {
		writeCheck(w, http.StatusConflict, "limit", err.Error())
		return
	}
	respondStatus(w, http.StatusCreated, map[string]any{"account": showAccount(a), "terminal": s.withClients(t)}, err)
}

// loginAccount opens a new login terminal for an existing account: a login
// whose refresh token expired, or a switch of the account to another login.
func (s *Server) loginAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaceGroup(w, r) {
		return
	}
	var body loginBody
	if !decode(w, r, 1<<10, &body, bodyRequired) {
		return
	}
	b, ok := s.workspaceBox(w, r)
	if !ok {
		return
	}
	if !body.valid(w) {
		return
	}
	a, ok := s.workspaceAccount(w, r, b, r.PathValue("aid"))
	if !ok {
		return
	}
	c, ok := s.boxClient(w)
	if !ok {
		return
	}
	if !s.readyBox(w, r, c, b, &a) {
		return
	}
	s.startTerminal(w, r, b, &a, loginTerminal(b, a, body, actor(r, "operator")))
}
