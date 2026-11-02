package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Box is an agent's box row: its id is the box workspace's id.
type Box struct {
	ID        string    `json:"id"`
	Agent     string    `json:"agent"`
	CreatedAt time.Time `json:"created_at"`
}

// EnsureBox returns the agent's box row, creating it on first use.
func (s *Store) EnsureBox(ctx context.Context, agent string) (Box, error) {
	var b Box
	err := s.Pool.QueryRow(ctx, `INSERT INTO boxes (agent) VALUES ($1)
		ON CONFLICT (agent) DO UPDATE SET agent = EXCLUDED.agent
		RETURNING id::text, agent, created_at`, agent).Scan(&b.ID, &b.Agent, &b.CreatedAt)
	return b, err
}

// GetBox returns a box row by its id (the workspace id).
func (s *Store) GetBox(ctx context.Context, id string) (Box, error) {
	if !IsUUID(id) {
		return Box{}, ErrNotFound
	}
	var b Box
	err := s.Pool.QueryRow(ctx, `SELECT id::text, agent, created_at FROM boxes WHERE id = $1`, id).
		Scan(&b.ID, &b.Agent, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Box{}, ErrNotFound
	}
	return b, err
}

// BoxTerminal states and end reasons (docs/api.md, Terminals).
const (
	TerminalRunning = "running"
	TerminalExited  = "exited"

	EndExit         = "exit"
	EndKilled       = "killed"
	EndBoxRestarted = "box_restarted"
)

// MaxRunningTerminals is the running terminals a workspace may have
// (docs/workspace.md, Auth, scope and limits).
const MaxRunningTerminals = 8

// ErrLimit is a count limit reached.
var ErrLimit = errors.New("limit reached")

// BoxTerminal is a tmux session in a box.
type BoxTerminal struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Agent       string `json:"-"`
	Preset      string `json:"preset"`
	// Session is the tmux session's name in the box.
	Session   string  `json:"-"`
	Title     string  `json:"title"`
	Cwd       string  `json:"cwd"`
	State     string  `json:"state"`
	ExitCode  *int    `json:"exit_code"`
	EndReason *string `json:"end_reason"`
	Cols      int     `json:"cols"`
	Rows      int     `json:"rows"`
	// Clients is how many sockets are attached on the instance answering;
	// the API fills it.
	Clients        int        `json:"clients"`
	CreatedBy      string     `json:"created_by"`
	CreatedAt      time.Time  `json:"created_at"`
	LastAttachedAt *time.Time `json:"last_attached_at"`
	// LastOutputAt is not recorded yet; always null.
	LastOutputAt *time.Time `json:"last_output_at"`
	EndedAt      *time.Time `json:"ended_at"`
}

// NewBoxTerminal is a terminal to record.
type NewBoxTerminal struct {
	WorkspaceID string
	Preset      string
	Title       string
	Cwd         string
	Cols, Rows  int
	CreatedBy   string
}

const terminalCols = `t.id::text, t.workspace_id::text, b.agent, t.preset, t.session, t.title, t.cwd,
	t.exit_code, t.end_reason, t.cols, t.rows, t.created_by, t.created_at, t.last_attached_at, t.ended_at`

func scanTerminal(row pgx.Row) (BoxTerminal, error) {
	var t BoxTerminal
	err := row.Scan(&t.ID, &t.WorkspaceID, &t.Agent, &t.Preset, &t.Session, &t.Title, &t.Cwd,
		&t.ExitCode, &t.EndReason, &t.Cols, &t.Rows, &t.CreatedBy, &t.CreatedAt, &t.LastAttachedAt, &t.EndedAt)
	t.State = TerminalRunning
	if t.EndedAt != nil {
		t.State = TerminalExited
	}
	return t, err
}

// CreateTerminal records a terminal, refusing with ErrLimit when its
// workspace has MaxRunningTerminals running. The box row's lock makes the
// count and the insert one decision.
func (s *Store) CreateTerminal(ctx context.Context, n NewBoxTerminal) (BoxTerminal, error) {
	var t BoxTerminal
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var running int
		err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM terminals WHERE workspace_id = b.id AND ended_at IS NULL)
			FROM boxes b WHERE b.id = $1 FOR UPDATE`, n.WorkspaceID).Scan(&running)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if running >= MaxRunningTerminals {
			return fmt.Errorf("%w: %d terminals are running in this workspace, the most allowed", ErrLimit, running)
		}
		// The tmux session is named for the terminal: a preset and the
		// first 12 hex digits of its id.
		t, err = scanTerminal(tx.QueryRow(ctx, `WITH n AS (SELECT gen_random_uuid() AS id),
			t AS (INSERT INTO terminals (id, workspace_id, preset, session, title, cwd, cols, rows, created_by)
				SELECT n.id, $1, $2, $2 || '-' || substr(replace(n.id::text, '-', ''), 1, 12), $3, $4, $5, $6, $7 FROM n
				RETURNING *)
			SELECT `+terminalCols+` FROM t JOIN boxes b ON b.id = t.workspace_id`,
			n.WorkspaceID, n.Preset, n.Title, n.Cwd, n.Cols, n.Rows, n.CreatedBy))
		return err
	})
	return t, err
}

// GetTerminal returns a terminal.
func (s *Store) GetTerminal(ctx context.Context, id string) (BoxTerminal, error) {
	if !IsUUID(id) {
		return BoxTerminal{}, ErrNotFound
	}
	t, err := scanTerminal(s.Pool.QueryRow(ctx, `SELECT `+terminalCols+`
		FROM terminals t JOIN boxes b ON b.id = t.workspace_id WHERE t.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return BoxTerminal{}, ErrNotFound
	}
	return t, err
}

// ListTerminals lists a workspace's terminals, newest first; state is
// running, exited or "" for all.
func (s *Store) ListTerminals(ctx context.Context, workspaceID, state string) ([]BoxTerminal, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+terminalCols+`
		FROM terminals t JOIN boxes b ON b.id = t.workspace_id
		WHERE t.workspace_id = $1
		  AND ($2 = '' OR ($2 = 'running') = (t.ended_at IS NULL))
		ORDER BY t.created_at DESC, t.id DESC LIMIT 500`, workspaceID, state)
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

// EndTerminal records a running terminal's end. A terminal that has ended
// already is ErrConflict.
func (s *Store) EndTerminal(ctx context.Context, id, reason string, exitCode *int) (BoxTerminal, error) {
	if !IsUUID(id) {
		return BoxTerminal{}, ErrNotFound
	}
	t, err := scanTerminal(s.Pool.QueryRow(ctx, `WITH t AS (UPDATE terminals SET ended_at = now(), end_reason = $2, exit_code = $3
			WHERE id = $1 AND ended_at IS NULL RETURNING *)
		SELECT `+terminalCols+` FROM t JOIN boxes b ON b.id = t.workspace_id`, id, reason, exitCode))
	if errors.Is(err, pgx.ErrNoRows) {
		if _, gerr := s.GetTerminal(ctx, id); gerr != nil {
			return BoxTerminal{}, gerr
		}
		return BoxTerminal{}, ErrConflict
	}
	return t, err
}

// TerminalAttached records an attach and the size the client asked for.
func (s *Store) TerminalAttached(ctx context.Context, id string, cols, rows int) error {
	_, err := s.Pool.Exec(ctx, `UPDATE terminals SET last_attached_at = now(), cols = $2, rows = $3 WHERE id = $1`, id, cols, rows)
	return err
}

// ticketBytes is an attach ticket's entropy.
const ticketBytes = 32

func ticketSum(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// MintTicket stores a single-use attach ticket for a terminal, a person and
// an Origin, valid for ttl, and returns it. Only its sha256 is stored.
func (s *Store) MintTicket(ctx context.Context, terminalID, person, origin string, ttl time.Duration) (string, time.Time, error) {
	b := make([]byte, ticketBytes)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	ticket := base64.RawURLEncoding.EncodeToString(b)
	var exp time.Time
	err := s.Pool.QueryRow(ctx, `INSERT INTO terminal_tickets (sha256, terminal_id, person, origin, expires_at)
		VALUES ($1, $2, $3, $4, now() + $5::interval) RETURNING expires_at`,
		ticketSum(ticket), terminalID, person, origin, fmt.Sprintf("%d milliseconds", ttl.Milliseconds())).Scan(&exp)
	return ticket, exp, err
}

// TicketsSince counts the tickets a person minted since t (the rate limit).
func (s *Store) TicketsSince(ctx context.Context, person string, t time.Time) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM terminal_tickets WHERE person = $1 AND created_at > $2`, person, t).Scan(&n)
	return n, err
}

// RedeemTicket uses a ticket once: it must be unused, unexpired, and have
// been minted for this terminal and origin. Any failure is ErrNotFound, so a
// caller learns nothing about which check failed.
func (s *Store) RedeemTicket(ctx context.Context, ticket, terminalID, origin string) (person string, err error) {
	if !IsUUID(terminalID) {
		return "", ErrNotFound
	}
	err = s.Pool.QueryRow(ctx, `UPDATE terminal_tickets SET used_at = now()
		WHERE sha256 = $1 AND terminal_id = $2 AND origin = $3 AND used_at IS NULL AND expires_at > now()
		RETURNING person`, ticketSum(ticket), terminalID, origin).Scan(&person)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return person, err
}
