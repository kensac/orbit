-- +goose Up

-- Every agent spec ever loaded, by content hash. A run names the spec it ran
-- under, so it stays explainable after the agent's file changes.
CREATE TABLE agent_specs (
    sha         text PRIMARY KEY,
    agent       text NOT NULL,
    spec        jsonb NOT NULL,
    first_seen  timestamptz NOT NULL DEFAULT now()
);

-- The agents the running configuration defines. A removed agent keeps its row
-- with present = false, so its history stays attached to a name.
CREATE TABLE agents (
    name        text PRIMARY KEY,
    spec_sha    text NOT NULL REFERENCES agent_specs (sha),
    present     boolean NOT NULL DEFAULT true,
    enabled     boolean NOT NULL,
    first_seen  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- One wake of one agent. This table is also the queue: the executor claims
-- queued rows with FOR UPDATE SKIP LOCKED.
CREATE TABLE runs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent           text NOT NULL REFERENCES agents (name),
    spec_sha        text NOT NULL REFERENCES agent_specs (sha),
    trigger         text NOT NULL CHECK (trigger IN ('schedule', 'manual', 'message')),
    -- Which entry of the agent's triggers list fired, for schedule runs.
    trigger_index   integer,
    scheduled_for   timestamptz,
    prompt          text NOT NULL DEFAULT '',
    -- Claims so far. A run interrupted by a shutdown or a lost worker goes
    -- back to the queue and is claimed again; its earlier attempts' events
    -- and artifacts stay, keyed by attempt.
    attempt         integer NOT NULL DEFAULT 0,
    status          text NOT NULL DEFAULT 'queued'
                    CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    runner_kind     text,
    model           text,
    worker          text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    started_at      timestamptz,
    heartbeat_at    timestamptz,
    finished_at     timestamptz,
    error           text,
    summary         text,
    report          text,
    output          jsonb,
    usage           jsonb,
    cost_usd        numeric(12, 6),
    session_id      text,
    CHECK ((trigger = 'schedule') = (scheduled_for IS NOT NULL AND trigger_index IS NOT NULL))
);

-- One fire of one schedule becomes at most one run, however many times the
-- scheduler restarts or catches up.
CREATE UNIQUE INDEX runs_one_per_fire ON runs (agent, trigger_index, scheduled_for)
    WHERE trigger = 'schedule';
-- At most one running run per agent: two runs would race on its memory.
CREATE UNIQUE INDEX runs_one_running ON runs (agent) WHERE status = 'running';
CREATE INDEX runs_queue ON runs (created_at) WHERE status = 'queued';
CREATE INDEX runs_agent_recent ON runs (agent, created_at DESC);

-- The runner's event stream, in order: the transcript.
CREATE TABLE run_events (
    run_id   uuid NOT NULL REFERENCES runs (id),
    attempt  integer NOT NULL,
    seq      integer NOT NULL,
    at       timestamptz NOT NULL DEFAULT now(),
    kind     text NOT NULL,
    payload  jsonb NOT NULL,
    PRIMARY KEY (run_id, attempt, seq)
);

-- What a run read in (sources, memory, messages) and wrote out (its raw
-- final output), byte for byte.
CREATE TABLE artifacts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id      uuid NOT NULL REFERENCES runs (id),
    attempt     integer NOT NULL,
    direction   text NOT NULL CHECK (direction IN ('in', 'out')),
    name        text NOT NULL,
    media_type  text NOT NULL,
    sha256      text NOT NULL,
    bytes       bigint NOT NULL,
    content     bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, attempt, direction, name)
);

-- Every version of every agent's memory. The current one is the newest.
CREATE TABLE memory_versions (
    id          bigserial PRIMARY KEY,
    agent       text NOT NULL REFERENCES agents (name),
    run_id      uuid REFERENCES runs (id),
    content     text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX memory_versions_current ON memory_versions (agent, id DESC);

-- The inbox, both directions. A message to an agent is consumed by the first
-- run that starts after it; a message from an agent is a question it asked.
CREATE TABLE messages (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent            text NOT NULL REFERENCES agents (name),
    direction        text NOT NULL CHECK (direction IN ('to_agent', 'from_agent')),
    body             text NOT NULL,
    reply_to         uuid REFERENCES messages (id),
    -- from_agent: the run that asked. to_agent: the run that read it.
    run_id           uuid REFERENCES runs (id),
    author           text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CHECK (direction = 'to_agent' OR run_id IS NOT NULL)
);
CREATE INDEX messages_unread ON messages (agent, created_at)
    WHERE direction = 'to_agent' AND run_id IS NULL;

-- Actions agents proposed and what became of them.
CREATE TABLE actions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id       uuid NOT NULL REFERENCES runs (id),
    agent        text NOT NULL REFERENCES agents (name),
    type         text NOT NULL,
    params       jsonb NOT NULL,
    reason       text NOT NULL DEFAULT '',
    rule         text NOT NULL CHECK (rule IN ('auto', 'approve', 'deny')),
    status       text NOT NULL
                 CHECK (status IN ('pending', 'approved', 'rejected', 'denied', 'executing', 'executed', 'failed')),
    decided_by   text,
    decided_at   timestamptz,
    executed_at  timestamptz,
    result       jsonb,
    error        text,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX actions_pending ON actions (created_at) WHERE status = 'pending';
CREATE INDEX actions_approved ON actions (decided_at) WHERE status = 'approved';

-- Every notification attempt.
CREATE TABLE deliveries (
    id            bigserial PRIMARY KEY,
    run_id        uuid REFERENCES runs (id),
    action_id     uuid REFERENCES actions (id),
    channel       text NOT NULL,
    purpose       text NOT NULL CHECK (purpose IN ('report', 'failure', 'approval', 'question', 'action')),
    status        text NOT NULL CHECK (status IN ('sent', 'failed')),
    error         text,
    attempted_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX deliveries_run ON deliveries (run_id);

-- +goose Down
DROP TABLE deliveries;
DROP TABLE actions;
DROP TABLE messages;
DROP TABLE memory_versions;
DROP TABLE artifacts;
DROP TABLE run_events;
DROP TABLE runs;
DROP TABLE agents;
DROP TABLE agent_specs;
