package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Workspace is one snapshot of a conversation's workspace.
type Workspace struct {
	ID             int64  `json:"id"`
	ConversationID string `json:"conversation_id"`
	// RunID and Attempt are the run that stored it; null for a person's.
	RunID   *string `json:"run_id"`
	Attempt *int    `json:"attempt"`
	// Source is run or person; Author the person (null for the operator
	// token and for runs).
	Source    string    `json:"source"`
	Author    *string   `json:"author"`
	SHA256    string    `json:"sha256"`
	Bytes     int64     `json:"bytes"`
	Files     int       `json:"files"`
	CreatedAt time.Time `json:"created_at"`
	// Archive is the tar, when read with its content.
	Archive []byte `json:"-"`
}

// The sources of a snapshot.
const (
	SnapshotRun    = "run"
	SnapshotPerson = "person"
)

const snapshotCols = `id, conversation_id::text, run_id::text, attempt, source, author, sha256, bytes, files, created_at`

func scanSnapshot(row pgx.Row, archive bool) (*Workspace, error) {
	var w Workspace
	dst := []any{&w.ID, &w.ConversationID, &w.RunID, &w.Attempt, &w.Source, &w.Author, &w.SHA256, &w.Bytes, &w.Files, &w.CreatedAt}
	if archive {
		dst = append(dst, &w.Archive)
	}
	err := row.Scan(dst...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// workspaceLock is the advisory lock of a conversation's workspace. A
// person's write holds it exclusively from its busy check to its commit;
// a run's restore holds it shared while it reads the newest snapshot. So a
// write either commits before a run reads (and the run restores it) or
// sees the run running and is refused (ErrWorkspaceBusy): never stored after the
// run read and then buried under the run's own snapshot.
const workspaceLock = `hashtextextended('orbit.workspace:' || $1::text, 0)`

// PutWorkspace stores a run's snapshot unless the conversation's newest is
// a run's with the same sha256. After a person's write it is stored even
// unchanged: it records that a run has seen the person's version, which
// is how the next turn knows what the person changed (PersonChanges).
// stored reports whether a row was written.
func (s *Store) PutWorkspace(ctx context.Context, conversationID, runID string, attempt, files int, archive []byte) (stored bool, err error) {
	sum := sha256.Sum256(archive)
	sha := hex.EncodeToString(sum[:])
	// The conversation's workspace is created with its first snapshot.
	tag, err := s.Pool.Exec(ctx, `WITH t AS (INSERT INTO workspace_trees (scope, agent, conversation_id)
		    SELECT 'conversation', c.agent, c.id FROM conversations c WHERE c.id = $1
		    ON CONFLICT (conversation_id) DO NOTHING)
		INSERT INTO workspaces (conversation_id, run_id, attempt, sha256, bytes, files, archive)
		SELECT $1, $2, $3, $4, $5, $6, $7
		WHERE NOT EXISTS (SELECT 1 FROM (SELECT sha256, source FROM workspaces WHERE conversation_id = $1 ORDER BY id DESC LIMIT 1) w
		                  WHERE w.sha256 = $4 AND w.source = 'run')`,
		conversationID, runID, attempt, sha, int64(len(archive)), files, archive)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// LatestWorkspace returns a conversation's newest snapshot with its
// archive, or ErrNotFound when it has none. It holds the workspace's
// lock shared while it reads (workspaceLock).
func (s *Store) LatestWorkspace(ctx context.Context, conversationID string) (*Workspace, error) {
	var w *Workspace
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(`+workspaceLock+`)`, conversationID); err != nil {
			return err
		}
		var err error
		w, err = scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotCols+`, archive
			FROM workspaces WHERE conversation_id = $1 ORDER BY id DESC LIMIT 1`, conversationID), true)
		return err
	})
	return w, err
}

// PersonChanges returns, when a person wrote the conversation's newest
// snapshot, that snapshot and the newest one a run stored before it (nil
// when no run has), both with archives: what the person changed since the
// agent last saw the workspace. Both are nil when the newest is a run's or
// there is none.
func (s *Store) PersonChanges(ctx context.Context, conversationID string) (latest, seen *Workspace, err error) {
	latest, err = scanSnapshot(s.Pool.QueryRow(ctx, `SELECT `+snapshotCols+`, archive
		FROM workspaces WHERE conversation_id = $1 ORDER BY id DESC LIMIT 1`, conversationID), true)
	if errors.Is(err, ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil || latest.Source != SnapshotPerson {
		return nil, nil, err
	}
	seen, err = scanSnapshot(s.Pool.QueryRow(ctx, `SELECT `+snapshotCols+`, archive
		FROM workspaces WHERE conversation_id = $1 AND source = 'run' AND id < $2 ORDER BY id DESC LIMIT 1`,
		conversationID, latest.ID), true)
	if errors.Is(err, ErrNotFound) {
		return latest, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return latest, seen, nil
}

// ErrWorkspaceBusy is a person's write to a workspace while a run of its
// conversation is running: the run's own snapshot would follow it.
var ErrWorkspaceBusy = errors.New("a run of this conversation is running; try again when it ends")

// ErrStale is a person's write based on a snapshot that is no longer the
// newest; the caller reads the newest and applies the operation again.
var ErrStale = errors.New("the workspace changed since it was read")

// WorkspaceEventIn is one audit row a write records.
type WorkspaceEventIn struct {
	Op            string
	Path          string
	From          string
	VersionBefore string
	VersionAfter  string
	Bytes         *int64
	// Nonce is a signed upload URL's, which uploads once.
	Nonce string
}

// TrashIn is a path a write moves to the trash; its content stays in the
// write's base snapshot.
type TrashIn struct {
	Path  string
	Kind  string
	Bytes int64
}

// PersonWrite is one person's change to a conversation's workspace: a new
// snapshot, the events that describe it, the trash it fills and the item
// it restores.
type PersonWrite struct {
	WorkspaceID    string
	ConversationID string
	// BaseID is the snapshot the change was made to, 0 for none.
	BaseID  int64
	Archive []byte
	Files   int
	// Person is the verified person, "" for the operator token.
	Person    string
	RequestID string
	Events    []WorkspaceEventIn
	Trash     []TrashIn
	// Restored is the trash item the write restores, if any.
	Restored string
}

// WriteResult is what PutPersonWrite stored.
type WriteResult struct {
	Snapshot *Workspace
	Trash    []TrashItem
}

// PutPersonWrite stores a person's write in one transaction: ErrWorkspaceBusy while a
// run of the conversation is running, ErrStale when the base is no longer
// the newest snapshot, ErrConflict when a signed upload's nonce was used.
func (s *Store) PutPersonWrite(ctx context.Context, wr PersonWrite) (*WriteResult, error) {
	res := &WriteResult{}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Its own statement, so the checks below read what committed while
		// this waited for the lock.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+workspaceLock+`)`, wr.ConversationID); err != nil {
			return err
		}
		var busy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runs WHERE conversation_id = $1 AND status = 'running')`,
			wr.ConversationID).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return ErrWorkspaceBusy
		}
		var newest int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(id), 0) FROM workspaces WHERE conversation_id = $1`, wr.ConversationID).Scan(&newest); err != nil {
			return err
		}
		if newest != wr.BaseID {
			return ErrStale
		}
		sum := sha256.Sum256(wr.Archive)
		w, err := scanSnapshot(tx.QueryRow(ctx, `INSERT INTO workspaces (conversation_id, source, author, sha256, bytes, files, archive)
			VALUES ($1, 'person', NULLIF($2, ''), $3, $4, $5, $6) RETURNING `+snapshotCols,
			wr.ConversationID, wr.Person, hex.EncodeToString(sum[:]), int64(len(wr.Archive)), wr.Files, wr.Archive), false)
		if err != nil {
			return err
		}
		res.Snapshot = w
		for _, ev := range wr.Events {
			_, err := tx.Exec(ctx, `INSERT INTO workspace_events (workspace_id, person, op, path, from_path, version_before, version_after,
				bytes, request_id, snapshot_id, nonce)
				VALUES ($1, NULLIF($2, ''), $3, $4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), $8, NULLIF($9, ''), $10, NULLIF($11, ''))`,
				wr.WorkspaceID, wr.Person, ev.Op, ev.Path, ev.From, ev.VersionBefore, ev.VersionAfter, ev.Bytes, wr.RequestID, w.ID, ev.Nonce)
			if isUniqueViolation(err) {
				return ErrConflict
			}
			if err != nil {
				return err
			}
		}
		for _, t := range wr.Trash {
			if wr.BaseID == 0 {
				return errors.New("nothing to trash in a workspace without a snapshot")
			}
			item, err := scanTrash(tx.QueryRow(ctx, `INSERT INTO workspace_trash (workspace_id, path, kind, bytes, snapshot_id, trashed_by)
				VALUES ($1, $2, $3, $4, $5, NULLIF($6, '')) RETURNING `+trashCols,
				wr.WorkspaceID, t.Path, t.Kind, t.Bytes, wr.BaseID, wr.Person))
			if err != nil {
				return err
			}
			res.Trash = append(res.Trash, *item)
		}
		if wr.Restored != "" {
			tag, err := tx.Exec(ctx, `UPDATE workspace_trash SET restored_at = now(), restored_by = NULLIF($3, '')
				WHERE id = $1 AND workspace_id = $2`, wr.Restored, wr.WorkspaceID, wr.Person)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrNotFound
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// TrashItem is a path in a workspace's trash.
type TrashItem struct {
	ID         string     `json:"id"`
	Path       string     `json:"path"`
	Kind       string     `json:"kind"`
	Bytes      int64      `json:"bytes"`
	TrashedAt  time.Time  `json:"trashed_at"`
	By         *string    `json:"by"`
	RestoredAt *time.Time `json:"restored_at"`
	// SnapshotID is the snapshot that holds its content.
	SnapshotID int64 `json:"-"`
}

const trashCols = `id::text, path, kind, bytes, trashed_at, trashed_by, restored_at, snapshot_id`

func scanTrash(row pgx.Row) (*TrashItem, error) {
	var t TrashItem
	err := row.Scan(&t.ID, &t.Path, &t.Kind, &t.Bytes, &t.TrashedAt, &t.By, &t.RestoredAt, &t.SnapshotID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetTrashItem returns one item of a workspace's trash.
func (s *Store) GetTrashItem(ctx context.Context, workspaceID, id string) (*TrashItem, error) {
	if !IsUUID(id) {
		return nil, ErrNotFound
	}
	return scanTrash(s.Pool.QueryRow(ctx, `SELECT `+trashCols+` FROM workspace_trash WHERE workspace_id = $1 AND id = $2`, workspaceID, id))
}

// TrashCursor is a position in a trash listing (trashed_at, id), newest
// first.
type TrashCursor struct {
	At time.Time
	ID string
}

// ListTrash returns a workspace's trash, newest first, after a cursor.
// limit defaults to 100, at most 500; next is set when there are more.
func (s *Store) ListTrash(ctx context.Context, workspaceID string, after *TrashCursor, limit int) (items []TrashItem, next *TrashCursor, err error) {
	limit = clampLimit(limit, 100, 500)
	var at *time.Time
	var id *string
	if after != nil {
		at, id = &after.At, &after.ID
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+trashCols+` FROM workspace_trash
		WHERE workspace_id = $1 AND ($2::timestamptz IS NULL OR (trashed_at, id) < ($2, $3::uuid))
		ORDER BY trashed_at DESC, id DESC LIMIT $4`, workspaceID, at, id, limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		t, err := scanTrash(rows)
		if err != nil {
			return nil, nil, err
		}
		items = append(items, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(items) > limit {
		items = items[:limit]
		last := items[limit-1]
		next = &TrashCursor{At: last.TrashedAt, ID: last.ID}
	}
	return items, next, nil
}
