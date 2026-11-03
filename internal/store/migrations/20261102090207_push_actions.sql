-- +goose Up

-- One row per push of attention rows to one person (internal/push): which rows, under which notification tag (the row's key, or orbit-inbox for a summary). closed_at is set once, when a close push for that tag went to the person's devices because what it showed had closed. Rows are kept.
CREATE TABLE push_sent (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    attention_id uuid NOT NULL REFERENCES attention (id),
    author       text NOT NULL,
    tag          text NOT NULL,
    sent_at      timestamptz NOT NULL DEFAULT now(),
    closed_at    timestamptz
);
CREATE INDEX push_sent_open ON push_sent (sent_at) WHERE closed_at IS NULL;

-- A push action link (Approve, Reject, Deny on a notification) used: its nonce, shared by every button of one notification, so one notification decides at most once. Rows are kept.
CREATE TABLE push_action_uses (
    nonce     text PRIMARY KEY,
    person    text NOT NULL,
    kind      text NOT NULL CHECK (kind IN ('action', 'approval')),
    target_id uuid NOT NULL,
    decision  text NOT NULL,
    used_at   timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE push_action_uses;
DROP TABLE push_sent;
