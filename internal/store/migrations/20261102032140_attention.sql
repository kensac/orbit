-- +goose Up

-- Something waiting on a person: a pending approval, an open question, a
-- failed run, a plan to approve. One row per thing (key), written in the
-- transaction that created the thing; whether it is still open is read from
-- the thing itself (attention.go), so a decision anywhere closes it. push is
-- false for a run a person was watching (a reply to their message): the
-- inbox lists it, devices are not woken for it.
CREATE TABLE attention (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    key             text NOT NULL UNIQUE,
    kind            text NOT NULL CHECK (kind IN ('approval', 'question', 'failure', 'decision')),
    agent           text NOT NULL REFERENCES agents (name),
    run_id          uuid NOT NULL REFERENCES runs (id),
    conversation_id uuid NOT NULL,
    action_id       uuid REFERENCES actions (id),
    message_id      uuid REFERENCES messages (id),
    ticket_id       uuid REFERENCES tickets (id),
    plan_version    integer,
    -- What a notification may say: the agent and the kind of thing, never
    -- a report, a question or an error (those stay behind sign-in).
    title           text NOT NULL,
    push            boolean NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- Set once the push loop has handled it (sent, folded into a summary,
    -- or skipped because it closed first).
    pushed_at       timestamptz,
    dismissed_at    timestamptz,
    dismissed_by    text
);
CREATE INDEX attention_unpushed ON attention (created_at) WHERE push AND pushed_at IS NULL;
CREATE INDEX attention_undismissed ON attention (created_at DESC) WHERE dismissed_at IS NULL;

-- A device that asked for Web Push (a browser's PushSubscription). Never
-- deleted: unsubscribing, or the push service answering 404/410, sets
-- revoked_at; subscribing again with the same endpoint revives the row.
CREATE TABLE push_subscriptions (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    endpoint     text NOT NULL UNIQUE,
    p256dh       text NOT NULL,
    auth         text NOT NULL,
    author       text NOT NULL,
    label        text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    last_sent_at timestamptz,
    last_error   text,
    failures     integer NOT NULL DEFAULT 0,
    revoked_at   timestamptz
);

-- When a push last went out: ClaimPush holds the gap between two from it.
CREATE TABLE push_state (
    id           boolean PRIMARY KEY DEFAULT true CHECK (id),
    last_push_at timestamptz NOT NULL
);

-- +goose Down
DROP TABLE push_state;
DROP TABLE push_subscriptions;
DROP TABLE attention;
