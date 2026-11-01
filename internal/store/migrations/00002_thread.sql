-- +goose Up

-- The conversation thread (Store.Thread) pages an agent's messages by time,
-- and gathers each run's questions and actions by run.
CREATE INDEX messages_agent_recent ON messages (agent, created_at DESC);
CREATE INDEX messages_run ON messages (run_id) WHERE run_id IS NOT NULL;
CREATE INDEX actions_run ON actions (run_id);

-- One row per client-minted request id, so a retried send or decision
-- returns its first result instead of acting twice. Written in the same
-- transaction as what it records; a request that failed leaves no receipt.
CREATE TABLE receipts (
    agent        text NOT NULL REFERENCES agents (name),
    client_id    text NOT NULL CHECK (length(client_id) BETWEEN 1 AND 200),
    -- What was asked, so the same id reused for a different request is
    -- refused rather than answered with an unrelated result.
    request_sha  text NOT NULL,
    response     jsonb,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (agent, client_id)
);

-- +goose Down
DROP TABLE receipts;
DROP INDEX actions_run;
DROP INDEX messages_run;
DROP INDEX messages_agent_recent;
