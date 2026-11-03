-- +goose Up

-- Versioned just before 20261102032149_search (out of order is allowed):
-- TestSearchBackfillsRowsWrittenBeforeIt migrates to the version before
-- search and then claims runs with today's code, which sets lane.

-- A run's lane: what may run beside what. One run per agent at a time,
-- except a chat agent's, which run one per conversation (its conversations
-- are separate sessions with separate workspaces). The claim sets lane:
-- the conversation for a chat agent, '' otherwise. A row an older release
-- claimed has NULL, read as '', so its claims stay one per agent.
ALTER TABLE runs ADD COLUMN lane text;
CREATE UNIQUE INDEX runs_one_running_lane ON runs (agent, COALESCE(lane, '')) WHERE status = 'running';
DROP INDEX runs_one_running;

-- +goose Down
CREATE UNIQUE INDEX runs_one_running ON runs (agent) WHERE status = 'running';
DROP INDEX runs_one_running_lane;
ALTER TABLE runs DROP COLUMN lane;
