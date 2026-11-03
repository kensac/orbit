package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxConversationPages bounds the number of pages one chat can retain.
const MaxConversationPages = 100

// PublishedPage is the immutable HTML document published by one action.
type PublishedPage struct {
	ID             string    `json:"id"`
	ConversationID string    `json:"conversation_id"`
	Title          string    `json:"title"`
	HTML           string    `json:"html"`
	CreatedAt      time.Time `json:"created_at"`
}

// ConversationPage returns an executed page:publish action in the named
// conversation. The action's parameters are the stored page, so the HTML
// has one retained copy and a mismatched conversation reveals nothing.
func (s *Store) ConversationPage(ctx context.Context, conversationID, pageID string) (*PublishedPage, error) {
	if !IsUUID(conversationID) || !IsUUID(pageID) {
		return nil, ErrInvalid
	}
	var p PublishedPage
	err := s.Pool.QueryRow(ctx, `SELECT a.id::text, r.conversation_id::text, a.params->>'title', a.params->>'html', a.executed_at
		FROM actions a JOIN runs r ON r.id = a.run_id
		WHERE a.id = $1 AND r.conversation_id = $2 AND a.type = 'page:publish' AND a.status = 'executed'`, pageID, conversationID).
		Scan(&p.ID, &p.ConversationID, &p.Title, &p.HTML, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// PageCountForRun counts other executed or in-progress pages in the run's
// conversation. Excluding the current action lets concurrent calls reserve
// against one another without counting themselves.
func (s *Store) PageCountForRun(ctx context.Context, runID, actionID string) (int, error) {
	if !IsUUID(runID) || !IsUUID(actionID) {
		return 0, ErrInvalid
	}
	var count int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM actions a JOIN runs r ON r.id = a.run_id
		WHERE r.conversation_id = (SELECT conversation_id FROM runs WHERE id = $1)
		  AND a.type = 'page:publish' AND a.status IN ('executed', 'executing') AND a.id <> $2`, runID, actionID).Scan(&count)
	return count, err
}
