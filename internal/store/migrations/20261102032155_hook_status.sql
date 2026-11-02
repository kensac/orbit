-- +goose Up

-- hook_status: what the run's Claude Code hooks last reported about its
-- turn (status.Snapshot: state, kind, detail, event, at, attempt), written
-- by the attempt holding the claim. NULL when no hook reported: another
-- backend, or a run from before the hooks posted. The agent status the API
-- shows is derived from it and the run's row (internal/status).
ALTER TABLE runs ADD COLUMN hook_status jsonb;

-- +goose Down
ALTER TABLE runs DROP COLUMN hook_status;
