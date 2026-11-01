-- +goose Up

-- A run may continue a backend session (spec `session`), and an agent may ask
-- to be woken again (the `continue` action). Both are recorded on the run.
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'message', 'continue'));

ALTER TABLE runs
    -- How this attempt used its backend session: a new one, a resumed
    -- (forked) copy of session_parent, or a turn fed to the live process
    -- that holds session_parent. Set when the attempt starts, before the
    -- backend is called, so an interrupted attempt still says what it touched.
    ADD COLUMN session_mode text CHECK (session_mode IN ('fresh', 'resumed', 'live')),
    ADD COLUMN session_parent text,
    -- Runs in this session's chain, this one included; bounded by the engine.
    ADD COLUMN session_depth integer,
    -- The backend's running cost total for the session after this run. It
    -- reports a cumulative figure; cost_usd is this run's share of it.
    ADD COLUMN session_cost_usd numeric(12, 6),
    -- A continuation is not claimed before run_after.
    ADD COLUMN run_after timestamptz,
    -- The run whose continue action enqueued this one, and how many
    -- continuations in a row lead here (1 for the first).
    ADD COLUMN continues uuid REFERENCES runs (id),
    ADD COLUMN chain integer NOT NULL DEFAULT 0,
    ADD CONSTRAINT runs_continue_shape
        CHECK ((trigger = 'continue') = (continues IS NOT NULL AND run_after IS NOT NULL AND chain > 0));

-- An agent has at most one continuation waiting, so a run cannot pile them up.
CREATE UNIQUE INDEX runs_one_continuation ON runs (agent) WHERE trigger = 'continue' AND status = 'queued';
-- The resume lookup: an agent's latest finished run.
CREATE INDEX runs_agent_finished ON runs (agent, finished_at DESC) WHERE status IN ('succeeded', 'failed');

-- +goose Down
DROP INDEX runs_agent_finished;
DROP INDEX runs_one_continuation;
ALTER TABLE runs DROP CONSTRAINT runs_continue_shape;
ALTER TABLE runs
    DROP COLUMN chain,
    DROP COLUMN continues,
    DROP COLUMN run_after,
    DROP COLUMN session_cost_usd,
    DROP COLUMN session_depth,
    DROP COLUMN session_parent,
    DROP COLUMN session_mode;
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'message'));
