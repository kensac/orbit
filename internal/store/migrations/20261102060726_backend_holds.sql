-- +goose Up

-- A backend hold: a runner whose backend refused orbit (an auth failure, or a
-- rejected subscription window) holds the unattended runs of every agent on
-- it, so they wait queued instead of each failing and notifying. A hold is
-- closed by a probe that succeeds or by a person's run that does; rows are
-- kept, one open per runner.
CREATE TABLE backend_holds (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    runner           text NOT NULL,
    kind             text NOT NULL CHECK (kind IN ('auth', 'rate_limit')),
    reason           text NOT NULL,
    -- The run whose failure opened it.
    run_id           uuid REFERENCES runs (id),
    opened_at        timestamptz NOT NULL DEFAULT now(),
    -- When a rejected window resets, as the backend reported it.
    resets_at        timestamptz,
    next_probe_at    timestamptz NOT NULL,
    probes           integer NOT NULL DEFAULT 0,
    last_probe_at    timestamptz,
    last_probe_error text,
    closed_at        timestamptz,
    -- probe, or run:<id> for a person's run that succeeded.
    closed_by        text,
    CHECK ((closed_at IS NULL) = (closed_by IS NULL))
);
CREATE UNIQUE INDEX backend_holds_one_open ON backend_holds (runner) WHERE closed_at IS NULL;

-- +goose Down
DROP TABLE backend_holds;
