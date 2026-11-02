-- +goose Up

-- How far the person has read a conversation (POST /conversations/{id}/read,
-- or a thread fetched with mark_read=1). A run that finished after it is
-- unread on the board. Existing conversations start read, so the board does
-- not light up every old conversation on the first deploy.
ALTER TABLE conversations ADD COLUMN read_through timestamptz;
UPDATE conversations SET read_through = now();

-- One small record per change a client showing every conversation cares
-- about (GET /events): a run created or its status moved, a message, an
-- attention row opened or dismissed, an action created or decided, a task
-- created or moved, a conversation created, renamed, archived or read.
-- Written by the triggers below, so every writer (either release during a
-- deploy, a worker) records alike. xid is the writing transaction: a reader
-- returns only rows whose transaction is older than every one still open
-- (pg_snapshot_xmin), in (xid, id) order, so a row committed late is never
-- skipped by a client that already moved past a higher id. Rows are kept.
CREATE TABLE changes (
    id              bigserial PRIMARY KEY,
    xid             xid8 NOT NULL DEFAULT pg_current_xact_id(),
    kind            text NOT NULL CHECK (kind IN ('run', 'message', 'attention', 'action', 'task', 'conversation')),
    conversation_id uuid,
    run_id          uuid,
    at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX changes_cursor ON changes (xid, id);

-- +goose StatementBegin
CREATE FUNCTION orbit_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    CASE TG_TABLE_NAME
    WHEN 'runs' THEN
        IF TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM NEW.status THEN
            INSERT INTO changes (kind, conversation_id, run_id) VALUES ('run', NEW.conversation_id, NEW.id);
        END IF;
    WHEN 'messages' THEN
        INSERT INTO changes (kind, conversation_id, run_id) VALUES ('message', NEW.conversation_id, NEW.run_id);
    WHEN 'attention' THEN
        IF TG_OP = 'INSERT' OR OLD.dismissed_at IS DISTINCT FROM NEW.dismissed_at THEN
            INSERT INTO changes (kind, conversation_id, run_id) VALUES ('attention', NEW.conversation_id, NEW.run_id);
        END IF;
    WHEN 'actions' THEN
        IF TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM NEW.status THEN
            INSERT INTO changes (kind, conversation_id, run_id)
            SELECT 'action', r.conversation_id, r.id FROM runs r WHERE r.id = NEW.run_id;
        END IF;
    WHEN 'tasks' THEN
        IF TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM NEW.status THEN
            INSERT INTO changes (kind, conversation_id) VALUES ('task', NEW.conversation_id), ('task', NEW.parent_conversation_id);
        END IF;
    WHEN 'conversations' THEN
        IF TG_OP = 'INSERT' OR OLD.title IS DISTINCT FROM NEW.title OR OLD.archived IS DISTINCT FROM NEW.archived
           OR OLD.read_through IS DISTINCT FROM NEW.read_through THEN
            INSERT INTO changes (kind, conversation_id) VALUES ('conversation', NEW.id);
        END IF;
    END CASE;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER runs_change AFTER INSERT OR UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION orbit_change();
CREATE TRIGGER messages_change AFTER INSERT ON messages FOR EACH ROW EXECUTE FUNCTION orbit_change();
CREATE TRIGGER attention_change AFTER INSERT OR UPDATE ON attention FOR EACH ROW EXECUTE FUNCTION orbit_change();
CREATE TRIGGER actions_change AFTER INSERT OR UPDATE ON actions FOR EACH ROW EXECUTE FUNCTION orbit_change();
CREATE TRIGGER tasks_change AFTER INSERT OR UPDATE ON tasks FOR EACH ROW EXECUTE FUNCTION orbit_change();
CREATE TRIGGER conversations_change AFTER INSERT OR UPDATE ON conversations FOR EACH ROW EXECUTE FUNCTION orbit_change();

-- +goose Down
DROP TRIGGER conversations_change ON conversations;
DROP TRIGGER tasks_change ON tasks;
DROP TRIGGER actions_change ON actions;
DROP TRIGGER attention_change ON attention;
DROP TRIGGER messages_change ON messages;
DROP TRIGGER runs_change ON runs;
DROP FUNCTION orbit_change();
DROP TABLE changes;
ALTER TABLE conversations DROP COLUMN read_through;
