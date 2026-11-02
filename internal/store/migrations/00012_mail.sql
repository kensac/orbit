-- +goose Up

-- Every mail orbit read from its mailbox, keyed by Message-ID so a mail is acted on once however often it is fetched. posted: it became a message (in conversation_id, with the run it woke); ignored: reason says why (a sender not allowed, a failed DMARC check, no text). raw is the mail as fetched, attachments included, so nothing in it is lost.
CREATE TABLE mail_in (
    message_id       text PRIMARY KEY,
    received_at      timestamptz NOT NULL DEFAULT now(),
    from_addr        text NOT NULL,
    subject          text NOT NULL DEFAULT '',
    outcome          text NOT NULL CHECK (outcome IN ('posted', 'ignored')),
    reason           text,
    agent            text REFERENCES agents (name),
    conversation_id  uuid REFERENCES conversations (id),
    orbit_message_id uuid REFERENCES messages (id),
    run_id           uuid REFERENCES runs (id),
    attachments      jsonb NOT NULL DEFAULT '[]',
    raw              bytea NOT NULL,
    CHECK ((outcome = 'posted') = (orbit_message_id IS NOT NULL))
);
CREATE INDEX mail_in_orbit_message ON mail_in (orbit_message_id) WHERE orbit_message_id IS NOT NULL;
