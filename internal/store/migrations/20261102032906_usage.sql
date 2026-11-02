-- +goose Up

-- What the subscription usage endpoint answered (internal/usage, orbit.yaml
-- usage_poll), one row per poll: the body as received, or why there was
-- none. Parsed when read, so a change to the endpoint's shape is a code
-- change over rows already kept.
CREATE TABLE usage_samples (
    id           bigserial PRIMARY KEY,
    polled_at    timestamptz NOT NULL DEFAULT now(),
    worker       text,
    ok           boolean NOT NULL,
    http_status  integer,
    error        text,
    payload      jsonb,
    CHECK (ok = (payload IS NOT NULL AND error IS NULL))
);
CREATE INDEX usage_samples_recent ON usage_samples (polled_at DESC);
CREATE INDEX usage_samples_recent_ok ON usage_samples (polled_at DESC) WHERE ok;

-- A scheduled run of a priority: low agent held back while a subscription
-- window was above the threshold (defer_low_priority). One row per run:
-- the first and latest hold, how many times it was held, the window and
-- reading behind the latest, and when it was let go.
CREATE TABLE run_deferrals (
    run_id       uuid PRIMARY KEY REFERENCES runs (id),
    agent        text NOT NULL REFERENCES agents (name),
    first_at     timestamptz NOT NULL DEFAULT now(),
    last_at      timestamptz NOT NULL DEFAULT now(),
    times        integer NOT NULL DEFAULT 1,
    -- The window over the threshold, its utilization (0 to 1), the
    -- threshold, where the reading came from (poll or stream) and when it
    -- was observed.
    window_name  text NOT NULL,
    utilization  double precision NOT NULL,
    threshold    double precision NOT NULL,
    source       text NOT NULL CHECK (source IN ('poll', 'stream')),
    observed_at  timestamptz NOT NULL,
    -- The run's run_after as of the latest hold.
    held_until   timestamptz NOT NULL,
    released_at  timestamptz
);
CREATE INDEX run_deferrals_held ON run_deferrals (run_id) WHERE released_at IS NULL;

-- +goose Down
DROP TABLE run_deferrals;
DROP TABLE usage_samples;
