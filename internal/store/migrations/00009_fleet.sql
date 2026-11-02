-- +goose Up

-- The worker fleet (docs/ARCHITECTURE.md, Worker fleet). Workers read
-- everything they run from here and never a file.

-- Every version of orbit.yaml control has loaded (after defaults), as
-- agent_specs holds every agent version, and the one in force. It names
-- secrets only by variable.
CREATE TABLE deployment_specs (
    sha        text PRIMARY KEY,
    spec       jsonb NOT NULL,
    first_seen timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE deployment_current (
    id         boolean PRIMARY KEY DEFAULT true CHECK (id),
    sha        text NOT NULL REFERENCES deployment_specs (sha),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- Notifications a worker recorded instead of sending: it holds no channel
-- credential, so control sends them (adding the links only it can sign)
-- and records each attempt in deliveries.
CREATE TABLE outbox (
    id         bigserial PRIMARY KEY,
    agent      text NOT NULL REFERENCES agents (name),
    run_id     uuid REFERENCES runs (id),
    action_id  uuid REFERENCES actions (id),
    purpose    text NOT NULL,
    message    jsonb NOT NULL,
    worker     text,
    created_at timestamptz NOT NULL DEFAULT now(),
    claimed_at timestamptz,
    sent_at    timestamptz
);
CREATE INDEX outbox_unsent ON outbox (id) WHERE sent_at IS NULL;

-- Claude Code session transcripts, stored centrally so a conversation can
-- continue on any host: a gzipped tar of <id>.jsonl and <id>/ from the
-- CLI's projects directory, written after a run or a hand-back, written
-- back on the claiming host before a resume. One row per session id; a
-- resume always forks, so a stored transcript is never rewritten.
CREATE TABLE session_transcripts (
    session_id text PRIMARY KEY,
    agent      text NOT NULL REFERENCES agents (name),
    run_id     uuid NOT NULL REFERENCES runs (id),
    attempt    integer NOT NULL,
    worker     text,
    archive    bytea NOT NULL,
    bytes      bigint NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE session_transcripts;
DROP TABLE outbox;
DROP TABLE deployment_current;
DROP TABLE deployment_specs;
