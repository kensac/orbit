-- +goose Up

-- An agent's conversations: the person's threads with it, as many as they
-- open (the chat app's "New chat"). Memory, schedules and rules stay per
-- agent; the inbox, open questions, the conversation shown to a run and its
-- backend session are per conversation. A conversation is never deleted,
-- only archived.
CREATE TABLE conversations (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    agent             text NOT NULL REFERENCES agents (name),
    -- '' until the first message gives it one (store.DefaultTitle), unless
    -- a person names it.
    title             text NOT NULL DEFAULT '',
    -- The one conversation scheduled runs, manual runs and the agent-level
    -- API (/agents/{name}/thread, /messages) belong to.
    is_default        boolean NOT NULL DEFAULT false,
    archived          boolean NOT NULL DEFAULT false,
    created_at        timestamptz NOT NULL DEFAULT now(),
    last_activity_at  timestamptz NOT NULL DEFAULT now(),
    -- The target of (conversation_id, agent) foreign keys, so a message or
    -- run can only be in a conversation of its own agent.
    UNIQUE (id, agent),
    CHECK (NOT (is_default AND archived))
);
CREATE UNIQUE INDEX conversations_one_default ON conversations (agent) WHERE is_default;
-- The conversation list, newest activity first.
CREATE INDEX conversations_agent_activity ON conversations (agent, last_activity_at DESC, id DESC);

INSERT INTO conversations (agent, title, is_default, created_at, last_activity_at)
SELECT name, 'General', true, first_seen, now() FROM agents;

ALTER TABLE messages ADD COLUMN conversation_id uuid;
ALTER TABLE runs ADD COLUMN conversation_id uuid;
UPDATE messages m SET conversation_id = c.id FROM conversations c WHERE c.agent = m.agent AND c.is_default;
UPDATE runs r SET conversation_id = c.id FROM conversations c WHERE c.agent = r.agent AND c.is_default;
UPDATE conversations c SET last_activity_at = GREATEST(
    (SELECT max(created_at) FROM messages WHERE conversation_id = c.id),
    (SELECT max(created_at) FROM runs WHERE conversation_id = c.id),
    c.created_at);

-- A row written without a conversation (the previous release, during a
-- deploy; a schedule fire; a new agent's first run) goes to its agent's
-- default conversation, created on first use. Writing activity moves the
-- conversation up the list.
-- +goose StatementBegin
CREATE FUNCTION orbit_conversation_fill() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.conversation_id IS NULL THEN
        INSERT INTO conversations (agent, title, is_default) VALUES (NEW.agent, 'General', true)
        ON CONFLICT (agent) WHERE is_default DO NOTHING;
        SELECT id INTO NEW.conversation_id FROM conversations WHERE agent = NEW.agent AND is_default;
    END IF;
    UPDATE conversations SET last_activity_at = GREATEST(last_activity_at, NEW.created_at)
    WHERE id = NEW.conversation_id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER messages_conversation BEFORE INSERT ON messages
    FOR EACH ROW EXECUTE FUNCTION orbit_conversation_fill();
CREATE TRIGGER runs_conversation BEFORE INSERT ON runs
    FOR EACH ROW EXECUTE FUNCTION orbit_conversation_fill();

ALTER TABLE messages ALTER COLUMN conversation_id SET NOT NULL,
    ADD CONSTRAINT messages_conversation_fk FOREIGN KEY (conversation_id, agent) REFERENCES conversations (id, agent);
ALTER TABLE runs ALTER COLUMN conversation_id SET NOT NULL,
    ADD CONSTRAINT runs_conversation_fk FOREIGN KEY (conversation_id, agent) REFERENCES conversations (id, agent);

-- A conversation's thread page, its unread messages, and its session lookup.
CREATE INDEX messages_conversation_recent ON messages (conversation_id, created_at DESC);
CREATE INDEX runs_conversation_recent ON runs (conversation_id, created_at DESC);
CREATE INDEX runs_conversation_finished ON runs (conversation_id, finished_at DESC) WHERE status IN ('succeeded', 'failed');

-- +goose Down
DROP INDEX runs_conversation_finished;
DROP INDEX runs_conversation_recent;
DROP INDEX messages_conversation_recent;
DROP TRIGGER runs_conversation ON runs;
DROP TRIGGER messages_conversation ON messages;
DROP FUNCTION orbit_conversation_fill();
ALTER TABLE runs DROP COLUMN conversation_id;
ALTER TABLE messages DROP COLUMN conversation_id;
DROP TABLE conversations;
