package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Meter is a conversation's context and cost as of its latest finished run,
// for the thread (ThreadPage.Meter). Every figure is read from what the
// backend reported and orbit stored; none is estimated.
type Meter struct {
	// RunID is the latest finished run, which the session figures are of;
	// null before any run finished.
	RunID *string `json:"run_id"`
	// SessionID is that run's backend session: the one the conversation's
	// next run continues when it continues one.
	SessionID *string `json:"session_id"`
	// ContextTokens is what the model held after its last call in that run:
	// the last assistant message's input, cache read, cache creation and
	// output tokens. Null when no message reported usage.
	ContextTokens *int64 `json:"context_tokens"`
	// ContextWindow is the largest context window the run's models reported
	// (Claude Code's modelUsage contextWindow).
	ContextWindow *int64 `json:"context_window"`
	// SessionCostUSD is the backend's running total for that session.
	SessionCostUSD *float64 `json:"session_cost_usd"`
	// CostUSD and Tokens are every run of the conversation together.
	CostUSD float64     `json:"cost_usd"`
	Tokens  MeterTokens `json:"tokens"`
	Runs    int         `json:"runs"`
}

// MeterTokens are token counts as the backends report them.
type MeterTokens struct {
	Input         int64 `json:"input"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
	Output        int64 `json:"output"`
}

// runUsage and msgUsage read one usage object (Claude Code's and the
// Messages API's names, Codex's cached_input_tokens as a cache read) as the
// four counts: a run's stored usage, and an assistant message's.
const (
	runUsage = `coalesce((coalesce(r.usage->'usage', r.usage)->>'input_tokens')::bigint, 0),
	coalesce((coalesce(r.usage->'usage', r.usage)->>'cache_read_input_tokens')::bigint, (coalesce(r.usage->'usage', r.usage)->>'cached_input_tokens')::bigint, 0),
	coalesce((coalesce(r.usage->'usage', r.usage)->>'cache_creation_input_tokens')::bigint, 0),
	coalesce((coalesce(r.usage->'usage', r.usage)->>'output_tokens')::bigint, 0)`
	msgUsage = `coalesce((msg.u->>'input_tokens')::bigint, 0),
	coalesce((msg.u->>'cache_read_input_tokens')::bigint, (msg.u->>'cached_input_tokens')::bigint, 0),
	coalesce((msg.u->>'cache_creation_input_tokens')::bigint, 0),
	coalesce((msg.u->>'output_tokens')::bigint, 0)`
)

func conversationMeter(ctx context.Context, tx pgx.Tx, conversationID string) (*Meter, error) {
	m := &Meter{}
	// A run's usage is stored as {"usage", "model_usage", ...} by the CLI
	// runners and as the Messages API's usage itself by the anthropic one.
	if err := tx.QueryRow(ctx, `
		SELECT coalesce(sum(cost_usd), 0)::float8, count(*),
		       coalesce(sum(i), 0)::bigint, coalesce(sum(cr), 0)::bigint, coalesce(sum(cc), 0)::bigint, coalesce(sum(o), 0)::bigint
		FROM (SELECT r.cost_usd, t.* FROM runs r,
		      LATERAL (SELECT `+runUsage+`) t(i, cr, cc, o)
		      WHERE r.conversation_id = $1 AND r.finished_at IS NOT NULL) x`, conversationID).
		Scan(&m.CostUSD, &m.Runs, &m.Tokens.Input, &m.Tokens.CacheRead, &m.Tokens.CacheCreation, &m.Tokens.Output); err != nil {
		return nil, err
	}
	var i, cr, cc, o *int64
	err := tx.QueryRow(ctx, `
		WITH last AS (
		    SELECT r.id, r.attempt, r.session_id, r.session_cost_usd, r.usage FROM runs r
		    WHERE r.conversation_id = $1 AND r.finished_at IS NOT NULL
		    ORDER BY r.finished_at DESC, r.id DESC LIMIT 1),
		msg AS (
		    SELECT coalesce(e.payload->'message'->'usage', e.payload->'usage') AS u FROM run_events e, last
		    WHERE e.run_id = last.id AND e.attempt = last.attempt AND e.kind IN ('assistant', 'response')
		      AND jsonb_typeof(coalesce(e.payload->'message'->'usage', e.payload->'usage')) = 'object'
		    ORDER BY e.seq DESC LIMIT 1)
		SELECT last.id::text, last.session_id, last.session_cost_usd::float8,
		       (SELECT max((v->>'contextWindow')::bigint) FROM jsonb_each(
		            CASE WHEN jsonb_typeof(last.usage->'model_usage') = 'object' THEN last.usage->'model_usage' ELSE '{}' END) AS mu(k, v)),
		       u.*
		FROM last LEFT JOIN LATERAL (SELECT `+msgUsage+` FROM msg) u(i, cr, cc, o) ON true`, conversationID).
		Scan(&m.RunID, &m.SessionID, &m.SessionCostUSD, &m.ContextWindow, &i, &cr, &cc, &o)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if i != nil {
		n := *i + *cr + *cc + *o
		m.ContextTokens = &n
	}
	return m, nil
}
