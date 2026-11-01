-- +goose Up

-- The conversation thread (Store.Thread) pages an agent's messages by time,
-- and gathers each run's questions and actions by run.
CREATE INDEX messages_agent_recent ON messages (agent, created_at DESC);
CREATE INDEX messages_run ON messages (run_id) WHERE run_id IS NOT NULL;
CREATE INDEX actions_run ON actions (run_id);

-- +goose Down
DROP INDEX actions_run;
DROP INDEX messages_run;
DROP INDEX messages_agent_recent;
