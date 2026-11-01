-- +goose Up

-- A request or question a person filed (docs/tickets.md). Its discussion is
-- ordinary messages and runs, scoped to it by their ticket_id.
CREATE TABLE tickets (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind           text NOT NULL CHECK (kind IN ('question', 'request')),
    title          text NOT NULL CHECK (title <> ''),
    -- The agent that discusses it.
    agent          text NOT NULL REFERENCES agents (name),
    -- The repository a request changes ("owner/name").
    repo           text,
    status         text NOT NULL DEFAULT 'open'
                   CHECK (status IN ('open', 'planned', 'approved', 'building', 'review',
                                     'merging', 'merged', 'done', 'failed', 'abandoned')),
    author         text,
    -- The plan version a person approved; a later version never inherits it.
    approved_plan  integer,
    base_sha       text,
    branch         text,
    pr_number      integer,
    head_sha       text,
    merge_sha      text,
    -- Why it failed, while it is failed.
    error          text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    CHECK ((kind = 'request') = (repo IS NOT NULL)),
    -- A question is answered in its thread; it never gets a plan or a PR.
    CHECK (kind = 'request' OR status IN ('open', 'done', 'abandoned'))
);
CREATE INDEX tickets_recent ON tickets (created_at DESC);
CREATE INDEX tickets_status ON tickets (status, created_at DESC);

-- Every plan an agent proposed for a request, by version.
CREATE TABLE ticket_plans (
    ticket_id   uuid NOT NULL REFERENCES tickets (id),
    version     integer NOT NULL CHECK (version > 0),
    body        text NOT NULL CHECK (body <> ''),
    run_id      uuid REFERENCES runs (id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (ticket_id, version)
);

-- Every transition of every ticket. tickets.status is the latest of these.
CREATE TABLE ticket_events (
    id           bigserial PRIMARY KEY,
    ticket_id    uuid NOT NULL REFERENCES tickets (id),
    from_status  text,
    to_status    text NOT NULL,
    by           text,
    detail       jsonb,
    at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ticket_events_ticket ON ticket_events (ticket_id, id);

-- A ticket's thread. NULL is an agent's general thread. Every reader of a
-- thread (the inbox, open questions, the conversation, session resume, the
-- thread API) is scoped by it.
ALTER TABLE messages ADD COLUMN ticket_id uuid REFERENCES tickets (id);
ALTER TABLE runs ADD COLUMN ticket_id uuid REFERENCES tickets (id);
-- A ticket's thread page, as messages_agent_recent and runs_agent_recent
-- serve an agent's.
CREATE INDEX messages_ticket ON messages (ticket_id, created_at DESC) WHERE ticket_id IS NOT NULL;
CREATE INDEX runs_ticket ON runs (ticket_id, created_at DESC) WHERE ticket_id IS NOT NULL;

-- +goose Down
DROP INDEX runs_ticket;
DROP INDEX messages_ticket;
ALTER TABLE runs DROP COLUMN ticket_id;
ALTER TABLE messages DROP COLUMN ticket_id;
DROP TABLE ticket_events;
DROP TABLE ticket_plans;
DROP TABLE tickets;
