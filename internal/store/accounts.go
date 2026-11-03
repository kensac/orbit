package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxBoxAccounts is the accounts one box may have: each is a mount, and
// sandbox.MaxAccounts bounds those.
const MaxBoxAccounts = 16

// BoxAccount is a Claude account in a box: what orbit knows of its login,
// read from the box, never a token (docs/agents/security.md).
type BoxAccount struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Agent       string `json:"-"`
	// Slot names the account's volume and directory.
	Slot             string     `json:"slot"`
	Label            string     `json:"label"`
	LoggedIn         bool       `json:"logged_in"`
	Email            *string    `json:"email"`
	AccountUUID      *string    `json:"account_uuid"`
	OrgID            *string    `json:"org_id"`
	OrgName          *string    `json:"org_name"`
	SubscriptionType *string    `json:"subscription_type"`
	RateLimitTier    *string    `json:"rate_limit_tier"`
	AccessExpiresAt  *time.Time `json:"access_expires_at"`
	RefreshExpiresAt *time.Time `json:"refresh_expires_at"`
	CheckedAt        *time.Time `json:"checked_at"`
	// LastUsedAt is the newest attach of a terminal under the account.
	LastUsedAt *time.Time `json:"last_used_at"`
	// Usage is the newest observation of its rate-limit windows
	// (sandbox.AccountUsage), null before one.
	Usage     json.RawMessage `json:"usage"`
	CreatedBy string          `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
}

// AccountLogin is what a status read of the box found.
type AccountLogin struct {
	LoggedIn         bool
	Email            string
	AccountUUID      string
	OrgID            string
	OrgName          string
	SubscriptionType string
	RateLimitTier    string
	AccessExpiresAt  *time.Time
	RefreshExpiresAt *time.Time
}

const accountCols = `a.id::text, a.workspace_id::text, b.agent, a.slot, a.label, a.logged_in, a.email, a.account_uuid, a.org_id,
	a.org_name, a.subscription_type, a.rate_limit_tier, a.access_expires_at, a.refresh_expires_at, a.checked_at,
	(SELECT max(t.last_attached_at) FROM terminals t WHERE t.account_id = a.id), a.usage, a.created_by, a.created_at`

func scanAccount(row pgx.Row) (BoxAccount, error) {
	var a BoxAccount
	err := row.Scan(&a.ID, &a.WorkspaceID, &a.Agent, &a.Slot, &a.Label, &a.LoggedIn, &a.Email, &a.AccountUUID, &a.OrgID,
		&a.OrgName, &a.SubscriptionType, &a.RateLimitTier, &a.AccessExpiresAt, &a.RefreshExpiresAt, &a.CheckedAt,
		&a.LastUsedAt, &a.Usage, &a.CreatedBy, &a.CreatedAt)
	if a.Usage == nil {
		a.Usage = json.RawMessage("null")
	}
	return a, err
}

// ValidAccountLabel reports whether l can label an account.
func ValidAccountLabel(l string) bool { return len(l) >= 1 && len(l) <= 100 }

// CreateAccount records a new account in a box workspace. Its label is
// unique in the workspace (ErrConflict), and a workspace has at most
// MaxBoxAccounts (ErrLimit), counted under the box row's lock.
func (s *Store) CreateAccount(ctx context.Context, workspaceID, label, by string) (BoxAccount, error) {
	if !IsUUID(workspaceID) {
		return BoxAccount{}, ErrNotFound
	}
	var a BoxAccount
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM box_accounts WHERE workspace_id = b.id)
			FROM boxes b WHERE b.id = $1 FOR UPDATE`, workspaceID).Scan(&n)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if n >= MaxBoxAccounts {
			return fmt.Errorf("%w: this box has %d accounts, the most allowed", ErrLimit, n)
		}
		a, err = scanAccount(tx.QueryRow(ctx, `WITH n AS (SELECT gen_random_uuid() AS id),
			a AS (INSERT INTO box_accounts (id, workspace_id, slot, label, created_by)
				SELECT n.id, $1, substr(replace(n.id::text, '-', ''), 1, 12), $2, $3 FROM n RETURNING *)
			SELECT `+accountCols+` FROM a JOIN boxes b ON b.id = a.workspace_id`, workspaceID, label, by))
		return err
	})
	return a, uniqueConflict(err)
}

func uniqueConflict(err error) error {
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: %s", ErrConflict, "an account with that label exists in this workspace")
	}
	return err
}

// GetAccount returns an account.
func (s *Store) GetAccount(ctx context.Context, id string) (BoxAccount, error) {
	if !IsUUID(id) {
		return BoxAccount{}, ErrNotFound
	}
	a, err := scanAccount(s.Pool.QueryRow(ctx, `SELECT `+accountCols+` FROM box_accounts a JOIN boxes b ON b.id = a.workspace_id WHERE a.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return BoxAccount{}, ErrNotFound
	}
	return a, err
}

// ListAccounts lists a workspace's accounts, oldest first.
func (s *Store) ListAccounts(ctx context.Context, workspaceID string) ([]BoxAccount, error) {
	if !IsUUID(workspaceID) {
		return nil, ErrNotFound
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+accountCols+` FROM box_accounts a JOIN boxes b ON b.id = a.workspace_id
		WHERE a.workspace_id = $1 ORDER BY a.created_at, a.id`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoxAccount
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AccountSlots are a workspace's accounts' slots, the mounts its box needs.
func (s *Store) AccountSlots(ctx context.Context, workspaceID string) ([]string, error) {
	as, err := s.ListAccounts(ctx, workspaceID)
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Slot)
	}
	return out, err
}

// RenameAccount sets an account's label (ErrConflict when another account
// of the workspace has it).
func (s *Store) RenameAccount(ctx context.Context, id, label string) (BoxAccount, error) {
	if !IsUUID(id) {
		return BoxAccount{}, ErrNotFound
	}
	a, err := scanAccount(s.Pool.QueryRow(ctx, `WITH a AS (UPDATE box_accounts SET label = $2 WHERE id = $1 RETURNING *)
		SELECT `+accountCols+` FROM a JOIN boxes b ON b.id = a.workspace_id`, id, label))
	if errors.Is(err, pgx.ErrNoRows) {
		return BoxAccount{}, ErrNotFound
	}
	return a, uniqueConflict(err)
}

// RecordAccountLogin stores what a status read of the box found. A read
// that found no login keeps the identity fields (the email of the account
// that was logged in) and clears the login itself.
func (s *Store) RecordAccountLogin(ctx context.Context, id string, l AccountLogin) (BoxAccount, error) {
	if !IsUUID(id) {
		return BoxAccount{}, ErrNotFound
	}
	a, err := scanAccount(s.Pool.QueryRow(ctx, `WITH a AS (UPDATE box_accounts SET logged_in = $2,
			email = coalesce(nullif($3, ''), email), account_uuid = coalesce(nullif($4, ''), account_uuid),
			org_id = coalesce(nullif($5, ''), org_id), org_name = coalesce(nullif($6, ''), org_name),
			subscription_type = coalesce(nullif($7, ''), subscription_type), rate_limit_tier = coalesce(nullif($8, ''), rate_limit_tier),
			access_expires_at = $9, refresh_expires_at = $10, checked_at = now()
		WHERE id = $1 RETURNING *)
		SELECT `+accountCols+` FROM a JOIN boxes b ON b.id = a.workspace_id`,
		id, l.LoggedIn, l.Email, l.AccountUUID, l.OrgID, l.OrgName, l.SubscriptionType, l.RateLimitTier, l.AccessExpiresAt, l.RefreshExpiresAt))
	if errors.Is(err, pgx.ErrNoRows) {
		return BoxAccount{}, ErrNotFound
	}
	return a, err
}

// EndRunningTerminals records every running terminal of a workspace ended
// for reason (a box recreated under them) and returns them.
func (s *Store) EndRunningTerminals(ctx context.Context, workspaceID, reason string) ([]BoxTerminal, error) {
	if !IsUUID(workspaceID) {
		return nil, ErrNotFound
	}
	rows, err := s.Pool.Query(ctx, `WITH t AS (UPDATE terminals SET ended_at = now(), end_reason = $2
			WHERE workspace_id = $1 AND ended_at IS NULL RETURNING *)
		SELECT `+terminalCols+` FROM t`+terminalFrom, workspaceID, reason)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BoxTerminal
	for rows.Next() {
		t, err := scanTerminal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RunningTerminals counts a workspace's running terminals.
func (s *Store) RunningTerminals(ctx context.Context, workspaceID string) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM terminals WHERE workspace_id = $1 AND ended_at IS NULL`, workspaceID).Scan(&n)
	return n, err
}

// RecordAccountUsage stores an observation of an account's usage observed
// at at, unless the account holds a newer one already (a poll and a status
// line read racing). It reports whether it was stored.
func (s *Store) RecordAccountUsage(ctx context.Context, id string, usage json.RawMessage, at time.Time) (bool, error) {
	if !IsUUID(id) {
		return false, ErrNotFound
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE box_accounts SET usage = $2, usage_at = $3
		WHERE id = $1 AND (usage_at IS NULL OR usage_at < $3)`, id, usage, at)
	return tag.RowsAffected() == 1, err
}

// Boxes is every box row, for the work done on running boxes.
func (s *Store) Boxes(ctx context.Context) ([]Box, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id::text, agent, created_at FROM boxes ORDER BY agent`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Box
	for rows.Next() {
		var b Box
		if err := rows.Scan(&b.ID, &b.Agent, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
