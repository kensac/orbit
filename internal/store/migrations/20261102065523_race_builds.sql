-- +goose Up

-- A race (docs/tickets.md, Races): a request built two or three times
-- from one approved plan, each build a lane with its own branch
-- (orbit/<id>-a, -b, -c) and optionally its own model or effort, scored
-- by code, one of them picked by a person. race is set when filed;
-- picked_lane once a person picked, after which the ticket is that lane's.
ALTER TABLE tickets ADD COLUMN race integer CHECK (race BETWEEN 2 AND 3);
ALTER TABLE tickets ADD COLUMN picked_lane text;
ALTER TABLE tickets ADD CONSTRAINT tickets_race_request CHECK (race IS NULL OR kind = 'request');
ALTER TABLE tickets ADD CONSTRAINT tickets_picked_in_race CHECK (picked_lane IS NULL OR race IS NOT NULL);

-- One row per lane, written when the ticket is filed. variant is what the
-- lane's builds run with ({"model", "effort"}, either optional); checks is
-- the loop's latest reading of the lane PR's required checks at checks_head
-- before the pick; pr_closed_at is when the loop closed a lane PR that was
-- not picked (its branch is kept).
CREATE TABLE ticket_lanes (
    ticket_id     uuid NOT NULL REFERENCES tickets (id),
    lane          text NOT NULL CHECK (lane IN ('a', 'b', 'c')),
    variant       jsonb NOT NULL DEFAULT '{}',
    checks        text CHECK (checks IN ('pending', 'green', 'failed')),
    checks_head   text,
    pr_state      text CHECK (pr_state IN ('open', 'closed', 'merged')),
    checks_at     timestamptz,
    pr_closed_at  timestamptz,
    PRIMARY KEY (ticket_id, lane)
);
ALTER TABLE tickets ADD CONSTRAINT tickets_picked_lane_fk FOREIGN KEY (id, picked_lane) REFERENCES ticket_lanes (ticket_id, lane);

-- Each build's delivery, recorded with its outcome (store.FinishBuild,
-- FailBuild), so lanes compare without reading GitHub: the lane it
-- built, its branch, base, head and PR, its diff's size and paths, and whether
-- the diff policy passed (passed when it delivered, failed when the policy
-- refused the diff, NULL when the build ended before the policy ran).
ALTER TABLE ticket_builds ADD COLUMN lane text CHECK (lane IN ('a', 'b', 'c'));
ALTER TABLE ticket_builds ADD COLUMN branch text;
ALTER TABLE ticket_builds ADD COLUMN base_sha text;
ALTER TABLE ticket_builds ADD COLUMN head_sha text;
ALTER TABLE ticket_builds ADD COLUMN pr_number integer;
ALTER TABLE ticket_builds ADD COLUMN added integer;
ALTER TABLE ticket_builds ADD COLUMN deleted integer;
ALTER TABLE ticket_builds ADD COLUMN files jsonb;
ALTER TABLE ticket_builds ADD COLUMN diff_policy text CHECK (diff_policy IN ('passed', 'failed'));
CREATE INDEX ticket_builds_lane ON ticket_builds (ticket_id, lane, created_at DESC) WHERE lane IS NOT NULL;

-- +goose Down
DROP INDEX ticket_builds_lane;
ALTER TABLE ticket_builds DROP COLUMN diff_policy;
ALTER TABLE ticket_builds DROP COLUMN files;
ALTER TABLE ticket_builds DROP COLUMN deleted;
ALTER TABLE ticket_builds DROP COLUMN added;
ALTER TABLE ticket_builds DROP COLUMN pr_number;
ALTER TABLE ticket_builds DROP COLUMN head_sha;
ALTER TABLE ticket_builds DROP COLUMN base_sha;
ALTER TABLE ticket_builds DROP COLUMN branch;
ALTER TABLE ticket_builds DROP COLUMN lane;
ALTER TABLE tickets DROP CONSTRAINT tickets_picked_lane_fk;
DROP TABLE ticket_lanes;
ALTER TABLE tickets DROP CONSTRAINT tickets_picked_in_race;
ALTER TABLE tickets DROP CONSTRAINT tickets_race_request;
ALTER TABLE tickets DROP COLUMN picked_lane;
ALTER TABLE tickets DROP COLUMN race;
