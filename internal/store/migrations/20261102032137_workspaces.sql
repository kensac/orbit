-- +goose Up

-- A chat agent's workspace per conversation (docs/ARCHITECTURE.md, Chat
-- agents): the files it keeps between messages, as a tar after each run
-- that changed them. Every snapshot is kept; the newest is the workspace,
-- restored on whichever host claims the conversation's next run.
CREATE TABLE workspaces (
    id              bigserial PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES conversations (id),
    run_id          uuid NOT NULL REFERENCES runs (id),
    attempt         integer NOT NULL,
    sha256          text NOT NULL,
    bytes           bigint NOT NULL,
    files           integer NOT NULL,
    archive         bytea NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX workspaces_conversation ON workspaces (conversation_id, id DESC);

-- A person asked a running run to stop; its attempt interrupts the backend
-- and ends the run cancelled with what it had produced.
ALTER TABLE runs ADD COLUMN cancel_requested_at timestamptz;
ALTER TABLE runs ADD COLUMN cancel_requested_by text;

ALTER TABLE run_attempts DROP CONSTRAINT run_attempts_outcome_check;
ALTER TABLE run_attempts ADD CONSTRAINT run_attempts_outcome_check
    CHECK (outcome IN ('succeeded', 'failed', 'handed_back', 'lost', 'skipped', 'cancelled'));

-- +goose Down
ALTER TABLE run_attempts DROP CONSTRAINT run_attempts_outcome_check;
ALTER TABLE run_attempts ADD CONSTRAINT run_attempts_outcome_check
    CHECK (outcome IN ('succeeded', 'failed', 'handed_back', 'lost', 'skipped'));
ALTER TABLE runs DROP COLUMN cancel_requested_by;
ALTER TABLE runs DROP COLUMN cancel_requested_at;
DROP TABLE workspaces;
