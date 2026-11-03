package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kensac/orbit/internal/tokens"
)

// Tokens is what an attempt, or a run's attempts together, spent in tokens (internal/tokens): the prompt sent uncached (input), read from the prompt cache (cache_read), written to it (cache_creation), and generated (output). CacheHitRatio is cache_read over every prompt token, null when there were none. Model is the model that did most of the work (a run's: its latest reporting attempt's); ByModel splits an attempt's counts by model when the backend did.
type Tokens struct {
	Input         int64                   `json:"input"`
	Output        int64                   `json:"output"`
	CacheRead     int64                   `json:"cache_read"`
	CacheCreation int64                   `json:"cache_creation"`
	CacheHitRatio *float64                `json:"cache_hit_ratio"`
	NumTurns      int                     `json:"num_turns"`
	Model         string                  `json:"model,omitempty"`
	ByModel       map[string]tokens.Count `json:"by_model,omitempty"`
}

// hitRatioSQL is cache_read over every prompt token, six places, NULL when there were none.
func hitRatioSQL(input, cacheRead, cacheCreation string) string {
	return `round((` + cacheRead + `)::numeric / nullif((` + input + `) + (` + cacheRead + `) + (` + cacheCreation + `), 0), 6)`
}

// attemptTokens is an attempt row's Tokens as JSON, NULL when it reported none.
var attemptTokens = `CASE WHEN input_tokens IS NULL THEN NULL ELSE jsonb_build_object(
	'input', input_tokens, 'output', output_tokens, 'cache_read', cache_read_tokens, 'cache_creation', cache_creation_tokens,
	'cache_hit_ratio', ` + hitRatioSQL("input_tokens", "cache_read_tokens", "cache_creation_tokens") + `,
	'num_turns', num_turns, 'model', model, 'by_model', tokens_by_model) END`

// runTokens is a run's (aliased r) attempts' tokens together, NULL when none reported any.
var runTokens = `(SELECT CASE WHEN count(*) = 0 THEN NULL ELSE jsonb_build_object(
	'input', sum(t.input_tokens), 'output', sum(t.output_tokens),
	'cache_read', sum(t.cache_read_tokens), 'cache_creation', sum(t.cache_creation_tokens),
	'cache_hit_ratio', ` + hitRatioSQL("sum(t.input_tokens)", "sum(t.cache_read_tokens)", "sum(t.cache_creation_tokens)") + `,
	'num_turns', sum(t.num_turns),
	'model', (array_agg(t.model ORDER BY t.attempt DESC) FILTER (WHERE t.model IS NOT NULL))[1]) END
	FROM run_attempts t WHERE t.run_id = r.id AND t.input_tokens IS NOT NULL)`

// Prefix is the stable prefix a run's latest attempt sent (tokens.Prefix): its digest and each part's. PreviousRun is the newest earlier run of the same conversation that recorded one, and Changed the parts that differ from it (empty when none did); both are absent when there is no such run. A change to any part but charter is a prompt cache miss for everything after it.
type Prefix struct {
	SHA         string            `json:"sha"`
	Parts       map[string]string `json:"parts"`
	PreviousRun *string           `json:"previous_run,omitempty"`
	// PreviousSHA is the previous run's digest: Changed lists a part beyond
	// charter exactly when it differs from SHA.
	PreviousSHA *string  `json:"previous_sha,omitempty"`
	Changed     []string `json:"changed"`
}

// AttemptPrefix is the stable prefix one attempt sent.
type AttemptPrefix struct {
	SHA   string            `json:"sha"`
	Parts map[string]string `json:"parts"`
}

const attemptPrefix = `CASE WHEN prefix_sha IS NULL THEN NULL ELSE jsonb_build_object('sha', prefix_sha, 'parts', prefix_parts) END`

// runPrefix is a run's (aliased r) latest recorded prefix and the previous run's in its conversation.
const runPrefix = `(SELECT jsonb_build_object('sha', a.prefix_sha, 'parts', a.prefix_parts,
	        'previous_run', p.id, 'previous_sha', p.prefix_sha, 'previous_parts', p.prefix_parts)
	FROM run_attempts a
	LEFT JOIN LATERAL (
	    SELECT pr.id::text AS id, pa.prefix_sha, pa.prefix_parts FROM runs pr
	    JOIN LATERAL (SELECT x.prefix_sha, x.prefix_parts FROM run_attempts x
	                  WHERE x.run_id = pr.id AND x.prefix_sha IS NOT NULL ORDER BY x.attempt DESC LIMIT 1) pa ON true
	    WHERE pr.conversation_id = r.conversation_id AND pr.created_at < r.created_at
	    ORDER BY pr.created_at DESC LIMIT 1) p ON true
	WHERE a.run_id = r.id AND a.prefix_sha IS NOT NULL ORDER BY a.attempt DESC LIMIT 1)`

// decodePrefix reads runPrefix's JSON.
func decodePrefix(raw []byte) (*Prefix, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var v struct {
		Prefix
		PreviousParts map[string]string `json:"previous_parts"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("read a run's prefix: %w", err)
	}
	p := v.Prefix
	if p.PreviousRun != nil {
		p.Changed = tokens.ChangedParts(v.PreviousParts, p.Parts)
	}
	return &p, nil
}

// RecordPrefix records the stable prefix an attempt sends, before it is sent, so a failed attempt says what it tried. ErrConflict means the attempt no longer holds the run.
func (s *Store) RecordPrefix(ctx context.Context, runID string, attempt int, p tokens.Prefix) error {
	parts, err := json.Marshal(p.Parts)
	if err != nil {
		return err
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE run_attempts a SET prefix_sha = $3, prefix_parts = $4
		FROM runs r
		WHERE a.run_id = $1 AND a.attempt = $2 AND a.outcome IS NULL
		  AND r.id = a.run_id AND r.status = 'running' AND r.attempt = a.attempt`, runID, attempt, p.SHA, parts)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("record the prefix of run %s attempt %d: %w (not its running claim)", runID, attempt, ErrConflict)
	}
	return nil
}

// recordTokens writes an attempt's token counts; nothing when the backend reported none.
func recordTokens(ctx context.Context, db execer, runID string, attempt int, u *tokens.Usage) error {
	if u == nil {
		return nil
	}
	var byModel []byte
	if len(u.ByModel) > 0 {
		byModel, _ = json.Marshal(u.ByModel)
	}
	var model *string
	if u.Model != "" {
		model = &u.Model
	}
	_, err := db.Exec(ctx, `UPDATE run_attempts SET input_tokens = $3, output_tokens = $4, cache_read_tokens = $5,
		cache_creation_tokens = $6, num_turns = $7, model = $8, tokens_by_model = $9
		WHERE run_id = $1 AND attempt = $2`,
		runID, attempt, u.Input, u.Output, u.CacheRead, u.CacheCreation, u.NumTurns, model, byModel)
	return err
}
