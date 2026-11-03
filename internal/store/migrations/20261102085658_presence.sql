-- +goose Up

-- Which conversation each person's app is showing, from its heartbeat (POST /presence): one row per person, updated in place. Presence is not a record (it says only where someone was looking in the last minute), so it is the one table orbit overwrites; nothing reads its history. A push for the conversation a person's row names, written in the last 60 s, skips that person's devices (internal/push).
CREATE TABLE presence (
    person          text PRIMARY KEY,
    conversation_id uuid,
    seen_at         timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE presence;
