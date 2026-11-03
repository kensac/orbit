-- +goose Up

-- A permission request a chat run's Claude Code process sent (can_use_tool) that waits for its person (docs/api.md, Approvals). The CLI's own account of the ask is kept as it sent it. A row is decided once (a guarded UPDATE from pending); one nobody answered by expires_at is denied, never allowed. Rows are kept.
CREATE TABLE approvals (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id               uuid NOT NULL REFERENCES runs (id),
    attempt              integer NOT NULL,
    conversation_id      uuid NOT NULL REFERENCES conversations (id),
    agent                text NOT NULL,
    request_id           text NOT NULL,
    kind                 text NOT NULL CHECK (kind IN ('tool', 'plan')),
    tool                 text NOT NULL,
    input                jsonb NOT NULL,
    level                text NOT NULL,
    decision_reason      text NOT NULL DEFAULT '',
    decision_reason_type text NOT NULL DEFAULT '',
    blocked_path         text NOT NULL DEFAULT '',
    matched_rule         jsonb,
    suggestions          jsonb,
    title                text NOT NULL DEFAULT '',
    description          text NOT NULL DEFAULT '',
    conversation_grant   boolean NOT NULL DEFAULT true,
    state                text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'allowed', 'denied', 'expired', 'withdrawn')),
    scope                text CHECK (scope IN ('once', 'conversation')),
    message              text NOT NULL DEFAULT '',
    decided_by           text,
    decided_at           timestamptz,
    expires_at           timestamptz NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (run_id, attempt, request_id),
    CONSTRAINT approvals_decided CHECK ((state = 'pending') = (decided_at IS NULL))
);
CREATE INDEX approvals_conversation ON approvals (conversation_id, created_at DESC);

-- +goose Down
DROP TABLE approvals;
