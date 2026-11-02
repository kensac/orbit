-- +goose Up

-- Every rerun of a PR's failed Actions jobs (docs/tickets.md, Fixing failed
-- checks): the ticket loop's own, one per head before it spends a fix, and
-- each a person asked for. A person's request is a row with done_at NULL
-- until the loop has asked GitHub; jobs is what it reran, error why it
-- could not.
CREATE TABLE ticket_check_reruns (
    id            bigserial PRIMARY KEY,
    ticket_id     uuid NOT NULL REFERENCES tickets (id),
    head_sha      text NOT NULL,
    requested_by  text NOT NULL,
    requested_at  timestamptz NOT NULL DEFAULT now(),
    done_at       timestamptz,
    jobs          jsonb,
    error         text
);
CREATE INDEX ticket_check_reruns_ticket ON ticket_check_reruns (ticket_id, id);
-- One request waits per ticket: a second click while the first waits is
-- the same request.
CREATE UNIQUE INDEX ticket_check_reruns_one_pending ON ticket_check_reruns (ticket_id) WHERE done_at IS NULL;

-- +goose Down
DROP TABLE ticket_check_reruns;
