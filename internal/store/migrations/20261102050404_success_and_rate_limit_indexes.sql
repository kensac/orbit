-- +goose Up

-- An agent's newest run that finished well, succeeded or skipped: /health's
-- last_success and the last-success gauge's startup seed. runs_agent_finished
-- covers succeeded and failed only, so these sorted every run of the agent.
CREATE INDEX runs_agent_last_success ON runs (agent, finished_at DESC) WHERE status IN ('succeeded', 'skipped');

-- RateLimitEvents (/usage): the rate_limit_event lines of the lookback, so
-- it reads only those instead of every event of the lookback's runs.
CREATE INDEX run_events_rate_limit ON run_events (at) WHERE kind = 'rate_limit_event';

-- +goose Down
DROP INDEX run_events_rate_limit;
DROP INDEX runs_agent_last_success;
