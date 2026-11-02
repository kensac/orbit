-- +goose Up

-- Full-text search over what orbit stores (Store.Search, GET /search). One
-- document per searchable piece: a person's message, an agent's question, a
-- run's report or error, the prose a backend narrated, one tool call's
-- input, one memory version. Documents are derived from those rows by the
-- triggers below, so every writer (either release during a deploy, a
-- worker) indexes alike, and they are never deleted, as the rows they come
-- from are not.
CREATE TABLE search_docs (
    id                 bigserial PRIMARY KEY,
    -- The row a document was derived from, so a re-derivation (a run's
    -- report written again, the backfill) replaces rather than duplicates:
    -- message:<id>, report:<run>, error:<run>, event:<run>:<attempt>:<seq>:<n>
    -- (n is the block within the line, 0 for its prose), memory:<id>.
    ref                text NOT NULL UNIQUE,
    kind               text NOT NULL
                       CHECK (kind IN ('message', 'question', 'report', 'error', 'narration', 'tool_call', 'memory')),
    agent              text NOT NULL,
    conversation_id    uuid,
    run_id             uuid,
    attempt            integer,
    seq                integer,
    message_id         uuid,
    memory_version_id  bigint,
    author             text,
    -- The tool a tool_call document is the input of.
    tool               text,
    at                 timestamptz NOT NULL,
    body               text NOT NULL,
    tsv                tsvector GENERATED ALWAYS AS
                       (setweight(to_tsvector('english', coalesce(tool, '')), 'B') || to_tsvector('english', body)) STORED
);
CREATE INDEX search_docs_tsv ON search_docs USING gin (tsv);
CREATE INDEX search_docs_agent_at ON search_docs (agent, at DESC);

-- orbit_search_clip bounds an indexed body: a tsvector is at most 1 MB, and
-- 200,000 characters of text stays well inside it whatever the words.
-- +goose StatementBegin
CREATE FUNCTION orbit_search_clip(t text) RETURNS text LANGUAGE sql IMMUTABLE AS $$
    SELECT left(t, 200000)
$$;
-- +goose StatementEnd

-- The searchable pieces of one transcript line, per backend's event kinds
-- (runner.Narrate reads the same shapes): Claude Code `assistant` and the
-- anthropic runner's `response` carry content blocks (text is prose,
-- tool_use a call; the StructuredOutput and submit calls are the report,
-- indexed from the run); codex `item.completed` carries prose and
-- `item.started` a command, search or MCP call.
-- +goose StatementBegin
CREATE FUNCTION orbit_search_event_docs(event_kind text, payload jsonb)
RETURNS TABLE (n integer, kind text, tool text, body text) LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    blocks jsonb;
    item jsonb;
    prose text := '';
    i integer := 0;
    b jsonb;
BEGIN
    IF event_kind = 'assistant' THEN
        blocks := payload->'message'->'content';
    ELSIF event_kind = 'response' THEN
        blocks := payload->'content';
    END IF;
    IF jsonb_typeof(blocks) = 'array' THEN
        FOR b IN SELECT value FROM jsonb_array_elements(blocks) LOOP
            i := i + 1;
            IF b->>'type' = 'text' AND btrim(coalesce(b->>'text', '')) <> '' THEN
                prose := prose || CASE WHEN prose = '' THEN '' ELSE E'\n\n' END || (b->>'text');
            ELSIF b->>'type' = 'tool_use' AND coalesce(b->>'name', '') NOT IN ('StructuredOutput', 'submit') THEN
                n := i; kind := 'tool_call'; tool := b->>'name'; body := coalesce(b->'input', '{}'::jsonb)::text;
                RETURN NEXT;
            END IF;
        END LOOP;
        IF prose <> '' THEN
            n := 0; kind := 'narration'; tool := NULL; body := prose;
            RETURN NEXT;
        END IF;
        RETURN;
    END IF;
    item := payload->'item';
    IF jsonb_typeof(item) <> 'object' THEN
        RETURN;
    END IF;
    IF event_kind = 'item.completed' AND item->>'type' IN ('reasoning', 'agent_message')
       AND btrim(coalesce(item->>'text', '')) <> '' AND left(btrim(item->>'text'), 1) <> '{' THEN
        -- A final agent_message that is JSON is the structured answer.
        n := 0; kind := 'narration'; tool := NULL; body := item->>'text';
        RETURN NEXT;
    ELSIF event_kind = 'item.started' AND item->>'type' = 'command_execution' THEN
        n := 1; kind := 'tool_call'; tool := 'shell'; body := coalesce(item->>'command', '');
        RETURN NEXT;
    ELSIF event_kind = 'item.started' AND item->>'type' = 'web_search' THEN
        n := 1; kind := 'tool_call'; tool := 'web_search'; body := coalesce(item->>'query', '');
        RETURN NEXT;
    ELSIF event_kind = 'item.started' AND item->>'type' = 'mcp_tool_call' THEN
        n := 1; kind := 'tool_call'; tool := concat_ws('.', item->>'server', item->>'tool');
        body := coalesce(item->'arguments', '{}'::jsonb)::text;
        RETURN NEXT;
    END IF;
END;
$$;
-- +goose StatementEnd

-- Indexing never fails the write it follows: a transcript line or a message
-- that could not be indexed is kept, and a warning names it.
-- +goose StatementBegin
CREATE FUNCTION orbit_search_index_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        INSERT INTO search_docs (ref, kind, agent, conversation_id, run_id, attempt, seq, tool, at, body)
        SELECT 'event:' || NEW.run_id || ':' || NEW.attempt || ':' || NEW.seq || ':' || d.n,
               d.kind, r.agent, r.conversation_id, NEW.run_id, NEW.attempt, NEW.seq, d.tool, NEW.at,
               orbit_search_clip(d.body)
        FROM runs r, orbit_search_event_docs(NEW.kind, NEW.payload) d
        WHERE r.id = NEW.run_id
        ON CONFLICT (ref) DO NOTHING;
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'search: run % event %/%: %', NEW.run_id, NEW.attempt, NEW.seq, SQLERRM;
    END;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER run_events_search AFTER INSERT ON run_events
    FOR EACH ROW WHEN (NEW.kind IN ('assistant', 'response', 'item.started', 'item.completed'))
    EXECUTE FUNCTION orbit_search_index_event();

-- +goose StatementBegin
CREATE FUNCTION orbit_search_index_message() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        INSERT INTO search_docs (ref, kind, agent, conversation_id, run_id, message_id, author, at, body)
        VALUES ('message:' || NEW.id, CASE NEW.direction WHEN 'to_agent' THEN 'message' ELSE 'question' END,
                NEW.agent, NEW.conversation_id, CASE WHEN NEW.direction = 'from_agent' THEN NEW.run_id END,
                NEW.id, NEW.author, NEW.created_at, orbit_search_clip(NEW.body))
        ON CONFLICT (ref) DO NOTHING;
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'search: message %: %', NEW.id, SQLERRM;
    END;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER messages_search AFTER INSERT ON messages
    FOR EACH ROW EXECUTE FUNCTION orbit_search_index_message();

-- A run's report (summary and report together) and its error, as of its
-- finish; a run finished again (never today) replaces them.
-- +goose StatementBegin
CREATE FUNCTION orbit_search_index_run() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        IF coalesce(NEW.summary, '') <> '' OR coalesce(NEW.report, '') <> '' THEN
            INSERT INTO search_docs (ref, kind, agent, conversation_id, run_id, attempt, at, body)
            VALUES ('report:' || NEW.id, 'report', NEW.agent, NEW.conversation_id, NEW.id, NEW.attempt,
                    coalesce(NEW.finished_at, now()),
                    orbit_search_clip(concat_ws(E'\n\n', NULLIF(NEW.summary, ''), NULLIF(NEW.report, ''))))
            ON CONFLICT (ref) DO UPDATE SET body = EXCLUDED.body, at = EXCLUDED.at, attempt = EXCLUDED.attempt;
        END IF;
        IF coalesce(NEW.error, '') <> '' THEN
            INSERT INTO search_docs (ref, kind, agent, conversation_id, run_id, attempt, at, body)
            VALUES ('error:' || NEW.id, 'error', NEW.agent, NEW.conversation_id, NEW.id, NEW.attempt,
                    coalesce(NEW.finished_at, now()), orbit_search_clip(NEW.error))
            ON CONFLICT (ref) DO UPDATE SET body = EXCLUDED.body, at = EXCLUDED.at, attempt = EXCLUDED.attempt;
        END IF;
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'search: run %: %', NEW.id, SQLERRM;
    END;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER runs_search AFTER UPDATE OF summary, report, error ON runs
    FOR EACH ROW WHEN (NEW.summary IS DISTINCT FROM OLD.summary OR NEW.report IS DISTINCT FROM OLD.report
                       OR NEW.error IS DISTINCT FROM OLD.error)
    EXECUTE FUNCTION orbit_search_index_run();

-- +goose StatementBegin
CREATE FUNCTION orbit_search_index_memory() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        INSERT INTO search_docs (ref, kind, agent, conversation_id, run_id, memory_version_id, at, body)
        SELECT 'memory:' || NEW.id, 'memory', NEW.agent, r.conversation_id, NEW.run_id, NEW.id, NEW.created_at,
               orbit_search_clip(NEW.content)
        FROM (SELECT 1) one LEFT JOIN runs r ON r.id = NEW.run_id
        WHERE btrim(NEW.content) <> ''
        ON CONFLICT (ref) DO NOTHING;
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'search: memory version %: %', NEW.id, SQLERRM;
    END;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER memory_versions_search AFTER INSERT ON memory_versions
    FOR EACH ROW EXECUTE FUNCTION orbit_search_index_memory();

-- The backfill: every row written before this migration. The triggers above
-- exist already and CREATE TRIGGER holds off concurrent writes to their
-- tables until this commits, so no row is missed or indexed twice.
INSERT INTO search_docs (ref, kind, agent, conversation_id, run_id, message_id, author, at, body)
SELECT 'message:' || m.id, CASE m.direction WHEN 'to_agent' THEN 'message' ELSE 'question' END,
       m.agent, m.conversation_id, CASE WHEN m.direction = 'from_agent' THEN m.run_id END,
       m.id, m.author, m.created_at, orbit_search_clip(m.body)
FROM messages m
ON CONFLICT (ref) DO NOTHING;

INSERT INTO search_docs (ref, kind, agent, conversation_id, run_id, attempt, at, body)
SELECT 'report:' || r.id, 'report', r.agent, r.conversation_id, r.id, r.attempt, coalesce(r.finished_at, r.created_at),
       orbit_search_clip(concat_ws(E'\n\n', NULLIF(r.summary, ''), NULLIF(r.report, '')))
FROM runs r WHERE coalesce(r.summary, '') <> '' OR coalesce(r.report, '') <> ''
ON CONFLICT (ref) DO NOTHING;

INSERT INTO search_docs (ref, kind, agent, conversation_id, run_id, attempt, at, body)
SELECT 'error:' || r.id, 'error', r.agent, r.conversation_id, r.id, r.attempt, coalesce(r.finished_at, r.created_at),
       orbit_search_clip(r.error)
FROM runs r WHERE coalesce(r.error, '') <> ''
ON CONFLICT (ref) DO NOTHING;

INSERT INTO search_docs (ref, kind, agent, conversation_id, run_id, memory_version_id, at, body)
SELECT 'memory:' || v.id, 'memory', v.agent, r.conversation_id, v.run_id, v.id, v.created_at, orbit_search_clip(v.content)
FROM memory_versions v LEFT JOIN runs r ON r.id = v.run_id
WHERE btrim(v.content) <> ''
ON CONFLICT (ref) DO NOTHING;

INSERT INTO search_docs (ref, kind, agent, conversation_id, run_id, attempt, seq, tool, at, body)
SELECT 'event:' || e.run_id || ':' || e.attempt || ':' || e.seq || ':' || d.n,
       d.kind, r.agent, r.conversation_id, e.run_id, e.attempt, e.seq, d.tool, e.at, orbit_search_clip(d.body)
FROM run_events e JOIN runs r ON r.id = e.run_id, orbit_search_event_docs(e.kind, e.payload) d
WHERE e.kind IN ('assistant', 'response', 'item.started', 'item.completed')
ON CONFLICT (ref) DO NOTHING;

-- +goose Down
DROP TRIGGER memory_versions_search ON memory_versions;
DROP TRIGGER runs_search ON runs;
DROP TRIGGER messages_search ON messages;
DROP TRIGGER run_events_search ON run_events;
DROP FUNCTION orbit_search_index_memory();
DROP FUNCTION orbit_search_index_run();
DROP FUNCTION orbit_search_index_message();
DROP FUNCTION orbit_search_index_event();
DROP FUNCTION orbit_search_event_docs(text, jsonb);
DROP FUNCTION orbit_search_clip(text);
DROP TABLE search_docs;
