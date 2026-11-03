-- +goose Up

-- A webhook delivery: a signed request to POST /hooks/{agent}/{trigger}
-- that became a run (trigger webhook). The body is kept as received, the
-- input of the run's webhook sources. A sender retries a delivery under the
-- same id, so (agent, trigger_name, delivery_id) is unique: a delivery is
-- acted on once, however often it arrives.
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'message', 'continue', 'build', 'learn', 'task', 'delegation', 'webhook'));

CREATE TABLE webhook_deliveries (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent        text NOT NULL REFERENCES agents (name),
    trigger_name text NOT NULL,
    delivery_id  text NOT NULL CHECK (delivery_id <> ''),
    -- The sender's event name (X-GitHub-Event), when it sends one.
    event        text,
    -- The sender's signed timestamp, when its scheme has one.
    sent_at      timestamptz,
    received_at  timestamptz NOT NULL DEFAULT now(),
    media_type   text NOT NULL,
    body         bytea NOT NULL,
    run_id       uuid NOT NULL UNIQUE REFERENCES runs (id)
);
CREATE UNIQUE INDEX webhook_deliveries_once ON webhook_deliveries (agent, trigger_name, delivery_id);

-- +goose Down
DROP TABLE webhook_deliveries;
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'message', 'continue', 'build', 'learn', 'task', 'delegation'));
