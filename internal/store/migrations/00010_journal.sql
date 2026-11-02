-- +goose Up

-- A run's typed journal (internal/journal): items derived from run_events
-- by code, append-only. A changed item is a new revision with its whole
-- body; nothing here is updated or deleted. run_events stays the
-- transcript this is derived from.
CREATE TABLE journal_items (
    run_id    uuid NOT NULL REFERENCES runs (id),
    -- The timeline: the first attempt of the chain of attempts that
    -- continue each other's sessions (run_attempts.recovered_from).
    epoch     integer NOT NULL,
    -- Row order within the epoch, from 1, without gaps: a reader's cursor.
    seq       integer NOT NULL,
    attempt   integer NOT NULL,
    -- The run_events row this was derived from; NULL for the revision
    -- that closes a turn when its attempt ended without a result.
    event_seq integer,
    item_id   text NOT NULL,
    revision  integer NOT NULL,
    kind      text NOT NULL,
    turn_id   text,
    parent_id text,
    at        timestamptz NOT NULL,
    -- The body's schema version (journal.Version when written).
    v         smallint NOT NULL,
    body      jsonb NOT NULL,
    -- Digests of the journal_bodies this row's bounded fields cut short,
    -- so a body is served only to a reader of a run that holds it.
    bodies    text[] NOT NULL DEFAULT '{}',
    PRIMARY KEY (run_id, epoch, seq),
    UNIQUE (run_id, epoch, item_id, revision),
    CHECK (seq >= 1 AND revision >= 1 AND attempt >= epoch)
);

-- The whole value behind a bounded head (a tool's output, a diff, a
-- thought), by content digest, shared by every row that cut it short.
CREATE TABLE journal_bodies (
    sha256  text PRIMARY KEY,
    bytes   bigint NOT NULL,
    content bytea NOT NULL
);

-- How far each attempt's events have been projected, and the projector's
-- state between events (the open turn). An attempt's live writes and the
-- backfill both advance this, under the run's advisory lock.
CREATE TABLE journal_sources (
    run_id  uuid NOT NULL REFERENCES runs (id),
    attempt integer NOT NULL,
    epoch   integer NOT NULL,
    through integer NOT NULL DEFAULT 0,
    state   jsonb NOT NULL DEFAULT '{}',
    -- The attempt ended and its open turn, if any, was closed.
    closed  boolean NOT NULL DEFAULT false,
    PRIMARY KEY (run_id, attempt)
);

-- The epoch an attempt belongs to: the root of its recovered_from chain.
-- An attempt with no run_attempts row (claimed before they existed) is its
-- own.
-- +goose StatementBegin
CREATE FUNCTION orbit_attempt_epoch(run uuid, att integer) RETURNS integer
LANGUAGE sql STABLE AS $$
    WITH RECURSIVE c(attempt, recovered_from) AS (
        SELECT attempt, recovered_from FROM run_attempts WHERE run_id = run AND attempt = att
        UNION ALL
        SELECT a.attempt, a.recovered_from FROM run_attempts a JOIN c ON a.run_id = run AND a.attempt = c.recovered_from
    )
    SELECT COALESCE(min(attempt), att) FROM c
$$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION orbit_attempt_epoch(uuid, integer);
DROP TABLE journal_sources;
DROP TABLE journal_bodies;
DROP TABLE journal_items;
