-- +goose Up

-- A build run (docs/tickets.md, The build run) is a run like any other, so
-- its events, artifacts, cost and stream are the run's.
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'message', 'continue', 'build'));

-- Which ticket each build run builds. purpose is build (the approved plan,
-- from the default branch) or fix (make failing checks pass, on top of the
-- PR's head); a fix carries the failure report code collected.
CREATE TABLE ticket_builds (
    run_id        uuid PRIMARY KEY REFERENCES runs (id),
    ticket_id     uuid NOT NULL REFERENCES tickets (id),
    purpose       text NOT NULL CHECK (purpose IN ('build', 'fix')),
    plan_version  integer NOT NULL,
    failure       jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((purpose = 'fix') = (failure IS NOT NULL))
);
CREATE INDEX ticket_builds_ticket ON ticket_builds (ticket_id, created_at DESC);

-- Every permission decision orbit's handler made for a build's CLI: one
-- row per can_use_tool request, written before the answer is sent, so an
-- answer that was never recorded was never an allow.
CREATE TABLE build_decisions (
    id          bigserial PRIMARY KEY,
    run_id      uuid NOT NULL REFERENCES runs (id),
    attempt     integer NOT NULL,
    request_id  text NOT NULL,
    tool        text NOT NULL,
    input       jsonb NOT NULL,
    allowed     boolean NOT NULL,
    message     text,
    at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, attempt, request_id)
);

-- Every sandbox a build attempt created, so a later attempt destroys what
-- an interrupted one left (sandboxd's own lifetime is the backstop) and a
-- build never holds two.
CREATE TABLE build_sandboxes (
    sandbox_id    text PRIMARY KEY,
    run_id        uuid NOT NULL REFERENCES runs (id),
    attempt       integer NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    destroyed_at  timestamptz
);
CREATE INDEX build_sandboxes_open ON build_sandboxes (run_id) WHERE destroyed_at IS NULL;

-- The github:merge action the ticket loop proposed for a ticket's PR.
ALTER TABLE tickets ADD COLUMN merge_action_id uuid REFERENCES actions (id);

-- One build at a time per repository, and one merge: two builds would race
-- on the same base, and for orbit itself two merges would ship in one
-- restart nobody reviewed together (docs/tickets.md, Self-hosting).
CREATE UNIQUE INDEX tickets_one_building ON tickets (repo) WHERE status = 'building';
CREATE UNIQUE INDEX tickets_one_merging ON tickets (repo) WHERE status IN ('merging', 'merged');

-- +goose Down
DROP INDEX tickets_one_merging;
DROP INDEX tickets_one_building;
ALTER TABLE tickets DROP COLUMN merge_action_id;
DROP TABLE build_sandboxes;
DROP TABLE build_decisions;
DROP TABLE ticket_builds;
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'message', 'continue'));
