-- Telegram updates are keyed by provider update_id so retries and concurrent
-- control instances cannot turn one update into two messages. Raw updates are
-- retained, including ignored ones, like mail_in.
CREATE TABLE telegram_in (
    update_id        bigint PRIMARY KEY,
    chat_id          text NOT NULL DEFAULT '',
    message_id       bigint NOT NULL DEFAULT 0,
    from_user_id     bigint NOT NULL DEFAULT 0,
    received_at      timestamptz NOT NULL DEFAULT now(),
    outcome          text NOT NULL CHECK (outcome IN ('posted', 'ignored')),
    reason           text,
    agent            text REFERENCES agents (name),
    conversation_id  uuid REFERENCES conversations (id),
    orbit_message_id uuid REFERENCES messages (id),
    run_id           uuid REFERENCES runs (id),
    raw              bytea NOT NULL,
    CHECK ((outcome = 'posted') = (orbit_message_id IS NOT NULL))
);
CREATE INDEX telegram_in_orbit_message ON telegram_in (orbit_message_id) WHERE orbit_message_id IS NOT NULL;

-- A person's Telegram reply names a sent bot message. Keep that provider id
-- mapped to its conversation so the reply can continue the right thread.
CREATE TABLE telegram_out (
    chat_id          text NOT NULL,
    message_id       bigint NOT NULL,
    conversation_id  uuid NOT NULL REFERENCES conversations (id),
    sent_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (chat_id, message_id)
);

-- +goose Down
DROP TABLE telegram_out;
DROP TABLE telegram_in;
