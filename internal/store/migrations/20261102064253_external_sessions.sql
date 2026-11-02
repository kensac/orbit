-- +goose Up

-- Sessions run outside orbit (a person's own Claude Code sessions on a
-- laptop), imported so orbit can search and learn from them
-- (POST /sessions/import, orbit import claude-sessions). A session is its
-- lines in order; each line's content is stored once, by digest, because a
-- resumed or forked session repeats its parent's lines. Nothing is deleted:
-- a session rewritten on disk (its imported prefix changed) is a new
-- generation of lines, and the old one stays.
CREATE TABLE external_sessions (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    source            text NOT NULL CHECK (source IN ('claude-code')),
    -- The source's own id: Claude Code's session id.
    session_id        text NOT NULL,
    cwd               text,
    branch            text,
    -- The repository the cwd is a checkout of (owner/name), as the
    -- importing client read it.
    repo              text,
    -- A title the session was given (a custom title, the CLI's summary).
    title             text,
    -- The person's first prompt in it.
    first_prompt      text,
    -- The current generation: its lines are the session.
    generation        integer NOT NULL DEFAULT 1,
    lines             integer NOT NULL DEFAULT 0,
    -- How much of the file the current generation holds: its byte length,
    -- and the SHA-256 of those bytes, so the next import reads only what
    -- was appended and knows the file is the one it read.
    offset_bytes      bigint NOT NULL DEFAULT 0,
    prefix_sha256     text NOT NULL,
    -- A chain over the line digests in order: sha256(previous || digest),
    -- from sha256 of nothing. Equal hashes, equal sessions.
    content_sha256    text NOT NULL,
    first_at          timestamptz,
    last_at           timestamptz NOT NULL,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (source, session_id)
);
CREATE INDEX external_sessions_last ON external_sessions (last_at DESC, id DESC);

-- One stored line, by the SHA-256 of its stored form (claudeimport.NormalizeLine).
CREATE TABLE external_line_contents (
    digest     text PRIMARY KEY,
    content    text NOT NULL,
    -- The line's type and timestamp, read from it at import.
    type       text,
    at         timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- A session's lines in order, per generation. n counts from 1.
CREATE TABLE external_session_lines (
    external_session_id uuid NOT NULL REFERENCES external_sessions (id),
    generation          integer NOT NULL,
    n                   integer NOT NULL,
    digest              text NOT NULL REFERENCES external_line_contents (digest),
    PRIMARY KEY (external_session_id, generation, n)
);
CREATE INDEX external_session_lines_digest ON external_session_lines (digest);

-- Search: one document per line that carries prose (the person's prompts,
-- the assistant's text, a summary), kind session. A line repeated by a fork
-- is indexed once, under the first session that had it.
ALTER TABLE search_docs ADD COLUMN external_session_id uuid;
ALTER TABLE search_docs DROP CONSTRAINT search_docs_kind_check;
ALTER TABLE search_docs ADD CONSTRAINT search_docs_kind_check
    CHECK (kind IN ('message', 'question', 'report', 'error', 'narration', 'tool_call', 'memory', 'memory_entry', 'session'));
ALTER TABLE search_docs DROP CONSTRAINT search_docs_check;
ALTER TABLE search_docs ADD CONSTRAINT search_docs_check
    CHECK ((agent IS NULL) = (kind = 'session' OR (kind = 'memory_entry' AND scope <> 'agent')));
CREATE INDEX search_docs_external_session ON search_docs (external_session_id) WHERE external_session_id IS NOT NULL;

-- The prose of one stored session line: a user line's text (not a tool
-- result, not a meta line), an assistant line's text blocks, a summary.
-- +goose StatementBegin
CREATE FUNCTION orbit_search_session_text(line jsonb) RETURNS text LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    content jsonb;
    out text := '';
    b jsonb;
BEGIN
    IF line->>'type' = 'summary' THEN
        RETURN coalesce(line->>'summary', '');
    END IF;
    IF line->>'type' NOT IN ('user', 'assistant') OR coalesce((line->>'isMeta')::boolean, false) THEN
        RETURN '';
    END IF;
    content := line->'message'->'content';
    IF jsonb_typeof(content) = 'string' THEN
        RETURN content#>>'{}';
    END IF;
    IF jsonb_typeof(content) <> 'array' THEN
        RETURN '';
    END IF;
    FOR b IN SELECT value FROM jsonb_array_elements(content) LOOP
        IF b->>'type' = 'text' AND btrim(coalesce(b->>'text', '')) <> '' THEN
            out := out || CASE WHEN out = '' THEN '' ELSE E'\n\n' END || (b->>'text');
        END IF;
    END LOOP;
    RETURN out;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION orbit_search_index_session_line() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        INSERT INTO search_docs (ref, kind, external_session_id, seq, at, body)
        SELECT 'session:' || c.digest, 'session', NEW.external_session_id, NEW.n, coalesce(c.at, c.created_at),
               orbit_search_clip(t.body)
        FROM external_line_contents c, orbit_search_session_text(c.content::jsonb) t(body)
        WHERE c.digest = NEW.digest AND btrim(t.body) <> ''
        ON CONFLICT (ref) DO NOTHING;
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'search: session % line %: %', NEW.external_session_id, NEW.n, SQLERRM;
    END;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER external_session_lines_search AFTER INSERT ON external_session_lines
    FOR EACH ROW EXECUTE FUNCTION orbit_search_index_session_line();

-- +goose Down
DROP TRIGGER external_session_lines_search ON external_session_lines;
DROP FUNCTION orbit_search_index_session_line();
DROP FUNCTION orbit_search_session_text(jsonb);
DELETE FROM search_docs WHERE kind = 'session';
DROP INDEX search_docs_external_session;
ALTER TABLE search_docs DROP CONSTRAINT search_docs_check;
ALTER TABLE search_docs ADD CONSTRAINT search_docs_check
    CHECK ((agent IS NULL) = (kind = 'memory_entry' AND scope <> 'agent'));
ALTER TABLE search_docs DROP CONSTRAINT search_docs_kind_check;
ALTER TABLE search_docs ADD CONSTRAINT search_docs_kind_check
    CHECK (kind IN ('message', 'question', 'report', 'error', 'narration', 'tool_call', 'memory', 'memory_entry'));
ALTER TABLE search_docs DROP COLUMN external_session_id;
DROP TABLE external_session_lines;
DROP TABLE external_line_contents;
DROP TABLE external_sessions;
