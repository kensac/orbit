package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConversationPermission is one version of a conversation's permission level (conversation_permissions). Level "" is the agent's own.
type ConversationPermission struct {
	ConversationID string     `json:"-"`
	Version        int        `json:"version"`
	Level          string     `json:"level"`
	SetBy          string     `json:"set_by"`
	SetAt          time.Time  `json:"set_at"`
	ExpiresAt      *time.Time `json:"expires_at"`
}

// InForce reports whether the version holds at now: it names a level and has not expired.
func (p *ConversationPermission) InForce(now time.Time) bool {
	return p != nil && p.Level != "" && (p.ExpiresAt == nil || now.Before(*p.ExpiresAt))
}

const permissionCols = `conversation_id::text, version, level, set_by, set_at, expires_at`

func scanPermission(row pgx.Row) (ConversationPermission, error) {
	var p ConversationPermission
	err := row.Scan(&p.ConversationID, &p.Version, &p.Level, &p.SetBy, &p.SetAt, &p.ExpiresAt)
	return p, err
}

// SetConversationPermission records a new version of the conversation's level: the conversation's row lock orders concurrent changes, so versions never collide. ErrNotFound for an unknown conversation.
func (s *Store) SetConversationPermission(ctx context.Context, conversationID, level, by string, expiresAt *time.Time) (ConversationPermission, error) {
	if !IsUUID(conversationID) {
		return ConversationPermission{}, ErrNotFound
	}
	var p ConversationPermission
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var id string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM conversations WHERE id = $1 FOR UPDATE`, conversationID).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var err error
		p, err = scanPermission(tx.QueryRow(ctx, `INSERT INTO conversation_permissions (conversation_id, version, level, set_by, expires_at)
			VALUES ($1, (SELECT coalesce(max(version), 0) + 1 FROM conversation_permissions WHERE conversation_id = $1), $2, $3, $4)
			RETURNING `+permissionCols, conversationID, level, by, expiresAt))
		return err
	})
	return p, err
}

// ConversationPermission is the conversation's newest version, nil when it has none (whether it is in force is InForce's).
func (s *Store) ConversationPermission(ctx context.Context, conversationID string) (*ConversationPermission, error) {
	if !IsUUID(conversationID) {
		return nil, nil
	}
	p, err := scanPermission(s.Pool.QueryRow(ctx, `SELECT `+permissionCols+` FROM conversation_permissions
		WHERE conversation_id = $1 ORDER BY version DESC LIMIT 1`, conversationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ConversationPermissions are the conversation's versions, newest first, at most limit.
func (s *Store) ConversationPermissions(ctx context.Context, conversationID string, limit int) ([]ConversationPermission, error) {
	out := []ConversationPermission{}
	if !IsUUID(conversationID) {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+permissionCols+` FROM conversation_permissions
		WHERE conversation_id = $1 ORDER BY version DESC LIMIT $2`, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPermission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
