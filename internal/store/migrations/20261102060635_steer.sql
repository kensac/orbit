-- +goose Up

-- How a person's message reaches its agent: 'queue' waits for the next run
-- (wakes one when asked), 'steer' goes into the conversation's running chat
-- turn. A steer message the turn took is steered_into that run (and read by
-- it, message_reads); one it did not take stays waiting, and the run's end
-- wakes a run for it (finishRun).
ALTER TABLE messages ADD COLUMN delivery text NOT NULL DEFAULT 'queue'
    CHECK (delivery IN ('queue', 'steer'));
ALTER TABLE messages ADD COLUMN steered_into uuid REFERENCES runs (id);
CREATE INDEX messages_steer ON messages (conversation_id, created_at)
    WHERE direction = 'to_agent' AND delivery = 'steer';

-- +goose Down
DROP INDEX messages_steer;
ALTER TABLE messages DROP COLUMN steered_into;
ALTER TABLE messages DROP COLUMN delivery;
