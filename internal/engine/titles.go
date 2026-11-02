package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/store"
)

// Conversation titles (orbit.yaml titles): a conversation is named by its
// first message's first line at once (store.DefaultTitle, code), then once
// by the titles runner from that message, unless a person titled it.

// titleBatch is how many conversations one pass titles, one call each: the
// pass runs every 30 s, and new conversations arrive far slower.
const titleBatch = 5

// titleTimeout bounds one title call: a one-line answer from a small model.
const titleTimeout = 90 * time.Second

// titleInput bounds the message the model reads: a title needs its start.
const titleInput = 4000

const titleSystem = "You name conversations. Reply with a title for the conversation that starts with the message below: " +
	"at most 6 words, in the message's language, naming its subject, with no quotes, no trailing punctuation and no Markdown. " +
	"Do not answer the message."

var titleSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["title"],` +
	`"properties":{"title":{"type":"string","maxLength":120}}}`)

// TitleConversations titles a batch of conversations still named by their
// first line. Without titles in orbit.yaml it does nothing and takes none, so
// turning it on later titles them then. A failed call keeps the first line,
// and is logged; the conversation is not tried again.
func (e *Engine) TitleConversations(ctx context.Context) error {
	d := e.Current().Config.Deployment
	if d.Titles == nil {
		return nil
	}
	rs, ok := d.Runners[d.Titles.Runner]
	if !ok {
		return nil
	}
	jobs, err := e.Store.ClaimTitles(ctx, titleBatch)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		r, err := e.NewRunner(d.Titles.Runner, rs, e.Env)
		if err != nil {
			return err
		}
		title, cost, err := e.modelTitle(ctx, r, j.Body)
		if err != nil {
			e.Log.Warn("title conversation", "conversation", j.ConversationID, "err", err)
		}
		if _, err := e.Store.SetModelTitle(context.WithoutCancel(ctx), j.ConversationID, title, cost); err != nil {
			return err
		}
	}
	return nil
}

// modelTitle asks r for a title, in an empty scratch directory and without a
// session, and cleans it as a first-line title is (store.DefaultTitle).
func (e *Engine) modelTitle(ctx context.Context, r runner.Runner, body string) (string, *float64, error) {
	if utf8.RuneCountInString(body) > titleInput {
		body = string([]rune(body)[:titleInput])
	}
	if err := os.MkdirAll(filepath.Join(e.DataDir, "titles"), 0o700); err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(filepath.Join(e.DataDir, "titles"), "t-")
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	ctx, cancel := context.WithTimeout(ctx, titleTimeout)
	defer cancel()
	res, err := r.Run(ctx, runner.Request{Workdir: dir, System: titleSystem, Prompt: body, OutputSchema: titleSchema},
		func(runner.Event) {})
	if err != nil {
		var cost *float64
		if res != nil {
			cost = res.CostUSD
		}
		return "", cost, err
	}
	var out struct {
		Title string `json:"title"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		return "", res.CostUSD, fmt.Errorf("the title answer is not {\"title\"}: %w", err)
	}
	return store.DefaultTitle(out.Title), res.CostUSD, nil
}
