-- +goose Up

-- What each attempt's backend calls spent in tokens (internal/tokens): the prompt sent uncached (input), read from the prompt cache (cache_read), written to it (cache_creation), and generated (output); the model that did most of it, the backend's turn count, and the same counts per model ({"<model>": {"input": n, ...}}) when the backend split them. NULL when the backend reported none (a gate's skip, a killed process).
ALTER TABLE run_attempts
    ADD COLUMN input_tokens          bigint,
    ADD COLUMN output_tokens         bigint,
    ADD COLUMN cache_read_tokens     bigint,
    ADD COLUMN cache_creation_tokens bigint,
    ADD COLUMN num_turns             integer,
    ADD COLUMN model                 text,
    ADD COLUMN tokens_by_model       jsonb,
    -- The digest of the stable prefix the attempt sent (runner.Prefix: what comes before the conversation in every request), and the digest of each part of it, so a run whose prefix differs from its conversation's previous run (a prompt cache miss) says which part changed.
    ADD COLUMN prefix_sha            text,
    ADD COLUMN prefix_parts          jsonb;

-- Runs finished before this recorded their latest attempt's usage on runs.usage only: Claude Code's as {"usage": ..., "model_usage": ..., "num_turns": n} (a build's as the bare usage), the API's and Codex's as the backend sent it. Those counts become the latest attempt's.
UPDATE run_attempts a SET
    input_tokens          = CASE WHEN r.runner_kind = 'codex' THEN greatest((u->>'input_tokens')::bigint - coalesce((u->>'cached_input_tokens')::bigint, 0), 0)
                                 ELSE coalesce((u->>'input_tokens')::bigint, 0) END,
    output_tokens         = coalesce((u->>'output_tokens')::bigint, 0),
    cache_read_tokens     = CASE WHEN r.runner_kind = 'codex' THEN coalesce((u->>'cached_input_tokens')::bigint, 0)
                                 ELSE coalesce((u->>'cache_read_input_tokens')::bigint, 0) END,
    cache_creation_tokens = CASE WHEN r.runner_kind = 'codex' THEN 0
                                 ELSE coalesce((u->>'cache_creation_input_tokens')::bigint, 0) END,
    num_turns             = coalesce((r.usage->>'num_turns')::integer, 1),
    model                 = coalesce(
        (SELECT m.key FROM jsonb_each(CASE WHEN jsonb_typeof(r.usage->'model_usage') = 'object' THEN r.usage->'model_usage' ELSE '{}'::jsonb END) m
         ORDER BY (m.value->>'costUSD')::numeric DESC NULLS LAST, m.key LIMIT 1),
        r.model)
FROM runs r, LATERAL (SELECT CASE WHEN jsonb_typeof(r.usage->'usage') = 'object' THEN r.usage->'usage' ELSE r.usage END AS u) x
WHERE a.run_id = r.id AND a.attempt = r.attempt AND a.input_tokens IS NULL
  AND jsonb_typeof(r.usage) = 'object' AND jsonb_typeof(u) = 'object' AND u ? 'input_tokens';

UPDATE run_attempts SET tokens_by_model = jsonb_build_object(model, jsonb_build_object(
    'input', input_tokens, 'output', output_tokens, 'cache_read', cache_read_tokens, 'cache_creation', cache_creation_tokens))
WHERE input_tokens IS NOT NULL AND tokens_by_model IS NULL AND model IS NOT NULL;

-- +goose Down
ALTER TABLE run_attempts
    DROP COLUMN input_tokens, DROP COLUMN output_tokens, DROP COLUMN cache_read_tokens, DROP COLUMN cache_creation_tokens,
    DROP COLUMN num_turns, DROP COLUMN model, DROP COLUMN tokens_by_model, DROP COLUMN prefix_sha, DROP COLUMN prefix_parts;
