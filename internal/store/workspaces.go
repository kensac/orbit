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
	ID             int64     `json:"id"`
	ConversationID string    `json:"conversation_id"`
	RunID          string    `json:"run_id"`
	Attempt        int       `json:"attempt"`
	SHA256         string    `json:"sha256"`
	Bytes          int64     `json:"bytes"`
	Files          int       `json:"files"`
	CreatedAt      time.Time `json:"created_at"`
	// Archive is the tar, when read with its content.
	Archive []byte `json:"-"`
}

// PutWorkspace stores a snapshot unless it is the conversation's newest
// already (same sha256). stored reports whether a row was written.
func (s *Store) PutWorkspace(ctx context.Context, conversationID, runID string, attempt, files int, archive []byte) (stored bool, err error) {
	sum := sha256.Sum256(archive)
	sha := hex.EncodeToString(sum[:])
	// The conversation's workspace is created with its first snapshot.
	tag, err := s.Pool.Exec(ctx, `WITH t AS (INSERT INTO workspace_trees (scope, agent, conversation_id)
		    SELECT 'conversation', c.agent, c.id FROM conversations c WHERE c.id = $1
		    ON CONFLICT (conversation_id) DO NOTHING)
		INSERT INTO workspaces (conversation_id, run_id, attempt, sha256, bytes, files, archive)
		SELECT $1, $2, $3, $4, $5, $6, $7
		WHERE NOT EXISTS (SELECT 1 FROM (SELECT sha256 FROM workspaces WHERE conversation_id = $1 ORDER BY id DESC LIMIT 1) w
		                  WHERE w.sha256 = $4)`,
		conversationID, runID, attempt, sha, int64(len(archive)), files, archive)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// LatestWorkspace returns a conversation's newest snapshot with its
// archive, or ErrNotFound when it has none.
func (s *Store) LatestWorkspace(ctx context.Context, conversationID string) (*Workspace, error) {
	var w Workspace
	err := s.Pool.QueryRow(ctx, `SELECT id, conversation_id::text, run_id::text, attempt, sha256, bytes, files, created_at, archive
		FROM workspaces WHERE conversation_id = $1 ORDER BY id DESC LIMIT 1`, conversationID).
		Scan(&w.ID, &w.ConversationID, &w.RunID, &w.Attempt, &w.SHA256, &w.Bytes, &w.Files, &w.CreatedAt, &w.Archive)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &w, err
}
