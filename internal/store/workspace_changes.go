package store

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// The change index of the snapshot backend (docs/api.md, Workspace: the
// change stream and history): what each snapshot changed against the one
// before it, computed by code (workspace.Changes) and stored once.

// WorkspaceChangeIn is one path a snapshot changed.
type WorkspaceChangeIn struct {
	Op      string
	Path    string
	From    string
	Kind    string
	Version string
}

// WorkspaceChange is one indexed change with the snapshot that made it.
type WorkspaceChange struct {
	SnapshotID int64
	Seq        int
	Path       string
	Op         string
	Kind       string
	From       *string
	Version    *string
	At         time.Time
	// Source, RunID and Author are the snapshot's (Workspace).
	Source string
	RunID  *string
	Author *string
}

const changeCols = `c.snapshot_id, c.seq, c.path, c.op, c.kind, c.from_path, c.version, w.created_at, w.source, w.run_id::text, w.author`

func collectChanges(rows pgx.Rows) ([]WorkspaceChange, error) {
	return pgx.CollectRows(rows, pgx.RowToStructByPos[WorkspaceChange])
}

// GetSnapshot is one snapshot without its archive.
func (s *Store) GetSnapshot(ctx context.Context, id int64) (*Workspace, error) {
	return scanSnapshot(s.Pool.QueryRow(ctx, `SELECT `+snapshotCols+` FROM workspaces WHERE id = $1`, id), false)
}

// PreviousSnapshot is the snapshot before id in its conversation, without
// its archive; ErrNotFound when id is the first.
func (s *Store) PreviousSnapshot(ctx context.Context, conversationID string, id int64) (*Workspace, error) {
	return scanSnapshot(s.Pool.QueryRow(ctx, `SELECT `+snapshotCols+` FROM workspaces
		WHERE conversation_id = $1 AND id < $2 ORDER BY id DESC LIMIT 1`, conversationID, id), false)
}

// UnindexedSnapshots are a conversation's snapshots without a change
// index, oldest first, at most limit.
func (s *Store) UnindexedSnapshots(ctx context.Context, conversationID string, limit int) ([]Workspace, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+snapshotCols+` FROM workspaces w
		WHERE w.conversation_id = $1 AND NOT EXISTS (SELECT 1 FROM workspace_change_sets s WHERE s.snapshot_id = w.id)
		ORDER BY w.id LIMIT $2`, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		w, err := scanSnapshot(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, *w)
	}
	return out, rows.Err()
}

// SnapshotRestores are the paths a person's write restored from the trash
// in a snapshot (its restore events).
func (s *Store) SnapshotRestores(ctx context.Context, snapshotID int64) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT path FROM workspace_events WHERE snapshot_id = $1 AND op = 'restore'`, snapshotID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// IndexSnapshot stores a snapshot's changes, once: when another instance
// indexed it first, nothing is written.
func (s *Store) IndexSnapshot(ctx context.Context, snap *Workspace, changes []WorkspaceChangeIn) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO workspace_change_sets (snapshot_id, conversation_id, changes)
			VALUES ($1, $2, $3) ON CONFLICT (snapshot_id) DO NOTHING`, snap.ID, snap.ConversationID, len(changes))
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		rows := make([][]any, 0, len(changes))
		for i, c := range changes {
			rows = append(rows, []any{snap.ID, i + 1, snap.ConversationID, c.Path, c.Op, c.Kind, nullIfEmpty(c.From), nullIfEmpty(c.Version)})
		}
		_, err = tx.CopyFrom(ctx, pgx.Identifier{"workspace_changes"},
			[]string{"snapshot_id", "seq", "conversation_id", "path", "op", "kind", "from_path", "version"}, pgx.CopyFromRows(rows))
		return err
	})
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// IndexedHead is the newest snapshot of a conversation up to which every
// snapshot is indexed (0 for none): changes are read only up to it, so a
// reader never passes a snapshot that is not indexed yet.
func (s *Store) IndexedHead(ctx context.Context, conversationID string) (int64, error) {
	var head int64
	err := s.Pool.QueryRow(ctx, `SELECT COALESCE(max(w.id), 0) FROM workspaces w
		WHERE w.conversation_id = $1 AND w.id < COALESCE((SELECT min(u.id) FROM workspaces u
		    WHERE u.conversation_id = $1 AND NOT EXISTS (SELECT 1 FROM workspace_change_sets s WHERE s.snapshot_id = u.id)), $2)`,
		conversationID, int64(math.MaxInt64)).Scan(&head)
	return head, err
}

// WorkspaceChanges are a conversation's indexed changes after (snapshot,
// seq), up to the snapshot upTo, oldest first, at most limit.
func (s *Store) WorkspaceChanges(ctx context.Context, conversationID string, afterSnapshot int64, afterSeq int, upTo int64, limit int) ([]WorkspaceChange, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+changeCols+` FROM workspace_changes c JOIN workspaces w ON w.id = c.snapshot_id
		WHERE c.conversation_id = $1 AND (c.snapshot_id, c.seq) > ($2, $3) AND c.snapshot_id <= $4
		ORDER BY c.snapshot_id, c.seq LIMIT $5`, conversationID, afterSnapshot, afterSeq, upTo, limit)
	if err != nil {
		return nil, err
	}
	return collectChanges(rows)
}

// PathHistory is a path's versions, newest first, before the snapshot
// before (0: from the newest): every change that left a file there.
// more reports an older one.
func (s *Store) PathHistory(ctx context.Context, conversationID, path string, before int64, limit int) (list []WorkspaceChange, more bool, err error) {
	limit = clampLimit(limit, 50, 500)
	rows, err := s.Pool.Query(ctx, `SELECT `+changeCols+` FROM workspace_changes c JOIN workspaces w ON w.id = c.snapshot_id
		WHERE c.conversation_id = $1 AND c.path = $2 AND c.kind = 'file' AND c.op <> 'trash' AND ($3 = 0 OR c.snapshot_id < $3)
		ORDER BY c.snapshot_id DESC LIMIT $4`, conversationID, path, before, limit+1)
	if err != nil {
		return nil, false, err
	}
	list, err = collectChanges(rows)
	if err != nil {
		return nil, false, err
	}
	if len(list) > limit {
		return list[:limit], true, nil
	}
	return list, false, nil
}

// RunSnapshots are the snapshots a run's changes lie between (docs/api.md,
// A reply's changes): After is the newest the run stored, Before the one
// before its first; a run that stored none changed nothing, and both are
// the snapshot the workspace was at when it finished (or now, while it
// runs). Either is nil for a workspace with no snapshot yet.
type RunSnapshots struct {
	RunID          string
	Agent          string
	Status         string
	ConversationID *string
	// WorkspaceID is nil for a run whose conversation has no workspace.
	WorkspaceID *string
	// Final is a run in a status that ends it: its snapshots are fixed.
	Final         bool
	Before, After *Workspace
}

// GetRunSnapshots returns a run's RunSnapshots.
func (s *Store) GetRunSnapshots(ctx context.Context, runID string) (*RunSnapshots, error) {
	if !IsUUID(runID) {
		return nil, ErrNotFound
	}
	rs := &RunSnapshots{RunID: runID}
	var finished *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT r.agent, r.status, r.conversation_id::text, t.id::text, r.finished_at
		FROM runs r LEFT JOIN workspace_trees t ON t.conversation_id = r.conversation_id WHERE r.id = $1`, runID).
		Scan(&rs.Agent, &rs.Status, &rs.ConversationID, &rs.WorkspaceID, &finished)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	switch rs.Status {
	case StatusSucceeded, StatusFailed, StatusCancelled, StatusSkipped:
		rs.Final = true
	}
	if rs.ConversationID == nil {
		return rs, nil
	}
	conv := *rs.ConversationID
	var first, last *int64
	if err := s.Pool.QueryRow(ctx, `SELECT min(id), max(id) FROM workspaces WHERE run_id = $1`, runID).Scan(&first, &last); err != nil {
		return nil, err
	}
	opt := func(w *Workspace, err error) (*Workspace, error) {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return w, err
	}
	if last != nil {
		if rs.After, err = s.GetSnapshot(ctx, *last); err != nil {
			return nil, err
		}
		rs.Before, err = opt(s.PreviousSnapshot(ctx, conv, *first))
		return rs, err
	}
	at, err := opt(scanSnapshot(s.Pool.QueryRow(ctx, `SELECT `+snapshotCols+` FROM workspaces
		WHERE conversation_id = $1 AND ($2::timestamptz IS NULL OR created_at <= $2) ORDER BY id DESC LIMIT 1`, conv, finished), false))
	if err != nil {
		return nil, err
	}
	rs.Before, rs.After = at, at
	return rs, nil
}

// ChangeSummary is a run's changes counted (docs/api.md, A reply's
// changes).
type ChangeSummary struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// RunChangeState is what a list of runs needs to show their summaries: the
// stored summary, or whether one can be computed.
type RunChangeState struct {
	Summary *ChangeSummary
	// Pending is a finished run with a workspace and no summary yet.
	Pending bool
}

// RunChangeStates returns, for each of the runs named, its stored summary,
// or Pending when it is finished, its conversation has a workspace and
// none is stored. A run with neither is left out (its changes are null).
func (s *Store) RunChangeStates(ctx context.Context, runIDs []string) (map[string]RunChangeState, error) {
	out := map[string]RunChangeState{}
	if len(runIDs) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT r.id::text, rc.files, rc.additions, rc.deletions,
		    rc.run_id IS NULL AND t.id IS NOT NULL AND r.status IN ('succeeded', 'failed', 'cancelled', 'skipped')
		FROM runs r
		LEFT JOIN run_changes rc ON rc.run_id = r.id
		LEFT JOIN workspace_trees t ON t.conversation_id = r.conversation_id
		WHERE r.id = ANY($1::uuid[])`, runIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var files, adds, dels *int
		var pending bool
		if err := rows.Scan(&id, &files, &adds, &dels, &pending); err != nil {
			return nil, err
		}
		switch {
		case files != nil:
			out[id] = RunChangeState{Summary: &ChangeSummary{Files: *files, Additions: *adds, Deletions: *dels}}
		case pending:
			out[id] = RunChangeState{Pending: true}
		}
	}
	return out, rows.Err()
}

// PutRunChanges stores a finished run's summary, once.
func (s *Store) PutRunChanges(ctx context.Context, runID string, before, after *Workspace, sum ChangeSummary) error {
	id := func(w *Workspace) *int64 {
		if w == nil {
			return nil
		}
		return &w.ID
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO run_changes (run_id, before_snapshot_id, after_snapshot_id, files, additions, deletions)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (run_id) DO NOTHING`, runID, id(before), id(after), sum.Files, sum.Additions, sum.Deletions)
	return err
}
