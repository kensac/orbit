package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/store"
)

func (e *Engine) pagePublishAction(ctx context.Context, c actions.Call, p actions.PagePublish) (json.RawMessage, error) {
	count, err := e.Store.PageCountForRun(ctx, c.RunID, c.ID)
	if err != nil {
		return nil, fmt.Errorf("page:publish: count conversation pages: %w", err)
	}
	if count >= store.MaxConversationPages {
		return nil, fmt.Errorf("page:publish: conversation has reached its limit of %d pages", store.MaxConversationPages)
	}
	// The action row already keeps the immutable title and HTML under its
	// conversation's run. Its id is the page id returned to the portfolio.
	return json.Marshal(struct {
		PageID string `json:"page_id"`
		Title  string `json:"title"`
	}{PageID: c.ID, Title: p.Title})
}
