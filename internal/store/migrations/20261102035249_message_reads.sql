-- +goose Up

-- Every attempt that read a person's message (StartRunContext), kept when
-- the attempt hands the message back (a failure, a requeue): released_at is
-- when it did. A message waits for the next run while no attempt holds it
-- (store.waitingMessage); messages.run_id names the run that read it last.
CREATE TABLE message_reads (
    message_id   uuid NOT NULL REFERENCES messages (id),
    run_id       uuid NOT NULL REFERENCES runs (id),
    attempt      integer NOT NULL,
    read_at      timestamptz NOT NULL DEFAULT now(),
    released_at  timestamptz,
    PRIMARY KEY (message_id, run_id, attempt)
);
CREATE INDEX message_reads_run ON message_reads (run_id) WHERE released_at IS NULL;
-- A message is held by at most one attempt at a time.
CREATE UNIQUE INDEX message_reads_one_holder ON message_reads (message_id) WHERE released_at IS NULL;

-- The reads before this table: a message a run holds names it in run_id.
-- Its attempt is the run's newest, the one that last read it.
INSERT INTO message_reads (message_id, run_id, attempt, read_at)
SELECT m.id, m.run_id, r.attempt, COALESCE(r.started_at, m.created_at)
FROM messages m JOIN runs r ON r.id = m.run_id
WHERE m.direction = 'to_agent';

-- +goose Down
DROP TABLE message_reads;
