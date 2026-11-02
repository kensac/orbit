-- +goose Up

-- One row per claim of a run. A run interrupted by a deploy, a crash or a
-- lost worker is claimed again; each claim is an attempt with its own
-- events and artifacts (keyed by attempt already), and this row says how it
-- ended and whether it continued the interrupted attempt's backend session.
CREATE TABLE run_attempts (
    run_id          uuid NOT NULL REFERENCES runs (id),
    attempt         integer NOT NULL,
    worker          text NOT NULL,
    -- The backend and spec this attempt ran under (runs holds the latest).
    runner_kind     text,
    spec_sha        text,
    started_at      timestamptz NOT NULL DEFAULT now(),
    ended_at        timestamptz,
    -- succeeded and failed end the run. handed_back: the attempt stopped
    -- itself (a drain's cap, or it lost its claim) and the run went back to
    -- the queue. lost: the reaper found it without a heartbeat.
    outcome         text CHECK (outcome IN ('succeeded', 'failed', 'handed_back', 'lost')),
    -- The backend session this attempt wrote to, recorded as soon as the
    -- backend's stream names it, so an interrupted attempt still says where
    -- its work is.
    session_id      text,
    -- The earlier attempt whose session this one resumed (forked) instead of
    -- starting over, or why it could not (recover_skipped).
    recovered_from  integer,
    recover_skipped text,
    -- This attempt's share of the backend's cost, and the backend's running
    -- session total after it. An interrupted attempt's figures come from
    -- what the backend saved in its transcript; a killed one may have none.
    cost_usd         numeric(12, 6),
    session_cost_usd numeric(12, 6),
    PRIMARY KEY (run_id, attempt),
    CHECK (recovered_from IS NULL OR recovered_from < attempt)
);

-- When an action was claimed for execution, so an action left executing is
-- told apart from one another process is executing right now: two
-- processes run at once during a deploy, and neither may fail the other's.
ALTER TABLE actions ADD COLUMN executing_since timestamptz;

-- +goose Down
ALTER TABLE actions DROP COLUMN executing_since;
DROP TABLE run_attempts;
