-- +goose Up

-- Who titled a conversation: first_line (code, from its first message,
-- store.DefaultTitle), model (the titles runner, engine/titles.go) or person
-- (named at creation or renamed). NULL for conversations from before this
-- column and for titles orbit gave otherwise (a task's, a mail's), which the
-- titles runner never changes. title_tried_at is when the titles runner took
-- it, once ever; title_cost_usd what that call cost.
ALTER TABLE conversations
    ADD COLUMN title_source text CHECK (title_source IN ('first_line', 'model', 'person')),
    ADD COLUMN title_tried_at timestamptz,
    ADD COLUMN title_cost_usd numeric;
CREATE INDEX conversations_untitled ON conversations (created_at)
    WHERE title_source = 'first_line' AND title_tried_at IS NULL;

-- An untitled conversation that a message names (postMessage's fill from
-- store.DefaultTitle, by either release during a deploy) is first_line,
-- unless someone has titled it before (a person who cleared their title) or
-- the write says who titles it (a person's rename).
-- +goose StatementBegin
CREATE FUNCTION orbit_title_source() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.title = '' AND NEW.title <> '' AND OLD.title_source IS NULL AND NEW.title_source IS NULL THEN
        NEW.title_source := 'first_line';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER conversations_title_source BEFORE UPDATE OF title ON conversations
    FOR EACH ROW EXECUTE FUNCTION orbit_title_source();

-- +goose Down
DROP TRIGGER conversations_title_source ON conversations;
DROP FUNCTION orbit_title_source();
DROP INDEX conversations_untitled;
ALTER TABLE conversations DROP COLUMN title_cost_usd, DROP COLUMN title_tried_at, DROP COLUMN title_source;
