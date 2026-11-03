-- +goose Up

-- Telegram update ids make polling replay safe, and the chat row carries its latest conversation.
CREATE TABLE telegram_chats (
    chat_id bigint PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES conversations(id),
    agent text NOT NULL REFERENCES agents(name),
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE telegram_in (
    update_id bigint PRIMARY KEY,
    chat_id bigint NOT NULL,
    user_id bigint NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    outcome text NOT NULL CHECK (outcome IN ('posted', 'ignored')),
    reason text,
    conversation_id uuid REFERENCES conversations(id),
    orbit_message_id uuid REFERENCES messages(id),
    run_id uuid REFERENCES runs(id),
    raw text NOT NULL DEFAULT '',
    CHECK ((outcome = 'posted') = (orbit_message_id IS NOT NULL))
);
CREATE INDEX telegram_in_message ON telegram_in (orbit_message_id) WHERE orbit_message_id IS NOT NULL;

-- +goose Down
DROP TABLE telegram_in;
DROP TABLE telegram_chats;
