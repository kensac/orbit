package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// WorkspaceTree is a workspace's identity (docs/workspace.md): one
// conversation's on the snapshot backend, one agent's in a box.
type WorkspaceTree struct {
	ID             string
	Scope          string
	Agent          string
	ConversationID *string
	CreatedAt      time.Time
}

const workspaceTreeCols = `id::text, scope, agent, conversation_id::text, created_at`

// EnsureConversationWorkspaces gives each named conversation a workspace if
// it has none, and returns every named conversation's workspace id.
// Conversations that do not exist are left out.
func (s *Store) EnsureConversationWorkspaces(ctx context.Context, conversationIDs []string) (map[string]string, error) {
	out := map[string]string{}
	if len(conversationIDs) == 0 {
		return out, nil
	}
	if _, err := s.Pool.Exec(ctx, `INSERT INTO workspace_trees (scope, agent, conversation_id)
		SELECT 'conversation', c.agent, c.id FROM conversations c WHERE c.id = ANY($1::uuid[])
		ON CONFLICT (conversation_id) DO NOTHING`, conversationIDs); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT conversation_id::text, id::text FROM workspace_trees WHERE conversation_id = ANY($1::uuid[])`, conversationIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c, id string
		if err := rows.Scan(&c, &id); err != nil {
			return nil, err
		}
		out[c] = id
	}
	return out, rows.Err()
}

// ConversationWorkspace returns a conversation's workspace, creating it
// first if it has none.
func (s *Store) ConversationWorkspace(ctx context.Context, conversationID string) (*WorkspaceTree, error) {
	if !IsUUID(conversationID) {
		return nil, ErrNotFound
	}
	if _, err := s.EnsureConversationWorkspaces(ctx, []string{conversationID}); err != nil {
		return nil, err
	}
	return s.scanTree(s.Pool.QueryRow(ctx, `SELECT `+workspaceTreeCols+` FROM workspace_trees WHERE conversation_id = $1`, conversationID))
}

// GetWorkspaceTree returns a workspace by id.
func (s *Store) GetWorkspaceTree(ctx context.Context, id string) (*WorkspaceTree, error) {
	if !IsUUID(id) {
		return nil, ErrNotFound
	}
	return s.scanTree(s.Pool.QueryRow(ctx, `SELECT `+workspaceTreeCols+` FROM workspace_trees WHERE id = $1`, id))
}

func (s *Store) scanTree(row pgx.Row) (*WorkspaceTree, error) {
	var t WorkspaceTree
	err := row.Scan(&t.ID, &t.Scope, &t.Agent, &t.ConversationID, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// DefaultConversationID is an agent's default conversation.
func (s *Store) DefaultConversationID(ctx context.Context, agent string) (string, error) {
	var id string
	err := s.Pool.QueryRow(ctx, `SELECT id::text FROM conversations WHERE agent = $1 AND is_default`, agent).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// LatestSnapshot is a conversation's newest snapshot without its archive,
// or ErrNotFound when it has none.
func (s *Store) LatestSnapshot(ctx context.Context, conversationID string) (*Workspace, error) {
	return scanSnapshot(s.Pool.QueryRow(ctx, `SELECT `+snapshotCols+`
		FROM workspaces WHERE conversation_id = $1 ORDER BY id DESC LIMIT 1`, conversationID), false)
}

// SnapshotArchive is one snapshot's tar, by id.
func (s *Store) SnapshotArchive(ctx context.Context, id int64) ([]byte, error) {
	var b []byte
	err := s.Pool.QueryRow(ctx, `SELECT archive FROM workspaces WHERE id = $1`, id).Scan(&b)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// WorkspaceEvent is one row of a workspace's audit.
type WorkspaceEvent struct {
	ID            int64     `json:"id"`
	At            time.Time `json:"at"`
	Person        *string   `json:"person"`
	Op            string    `json:"op"`
	Path          string    `json:"path"`
	From          *string   `json:"from"`
	VersionBefore *string   `json:"version_before"`
	VersionAfter  *string   `json:"version_after"`
	Bytes         *int64    `json:"bytes"`
	TerminalID    *string   `json:"terminal_id"`
	RunID         *string   `json:"run_id"`
	RequestID     *string   `json:"request_id"`
}

// WorkspaceEventPage is a page of a workspace's audit, newest first.
type WorkspaceEventPage struct {
	Events  []WorkspaceEvent
	HasMore bool
	// NextBefore is the id to pass as before for the next page.
	NextBefore int64
}

// ListWorkspaceEvents returns a workspace's audit, newest first, before an
// event id (0: from the newest). limit defaults to 100, at most 500.
func (s *Store) ListWorkspaceEvents(ctx context.Context, workspaceID string, before int64, limit int) (*WorkspaceEventPage, error) {
	limit = clampLimit(limit, 100, 500)
	rows, err := s.Pool.Query(ctx, `SELECT id, at, person, op, path, from_path, version_before, version_after, bytes,
		terminal_id::text, run_id::text, request_id
		FROM workspace_events WHERE workspace_id = $1 AND ($2 = 0 OR id < $2) ORDER BY id DESC LIMIT $3`,
		workspaceID, before, limit+1)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[WorkspaceEvent])
	if err != nil {
		return nil, err
	}
	page := &WorkspaceEventPage{Events: list}
	if len(list) > limit {
		page.Events, page.HasMore = list[:limit], true
		page.NextBefore = list[limit-1].ID
	}
	return page, nil
}
