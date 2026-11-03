-- +goose Up

-- Search on code and typos (Store.Search). Three additions to search_docs:
-- code_tsv, a second index of every document with its identifiers split into
-- pieces (parseConfig, parse_config, internal/store/search.go, store.Search),
-- so a query naming an identifier finds it however it was written; repo and
-- path, what a tool call's input names, for the repo: and path: operators; and
-- skill versions as documents of kind skill. search_terms is every word the
-- documents hold, which typo repair matches a word nothing has against.
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- orbit_search_code_text splits identifiers into their pieces: a camelCase
-- boundary (a lowercase letter or digit, then a capital; a capital, then a
-- capital and a lowercase letter, so HTTPServer is HTTP Server) and every ASCII
-- punctuation character become a space. store.codePieces is the same rule in
-- Go, held to this one by TestCodePiecesMatchTheDatabase.
-- +goose StatementBegin
CREATE FUNCTION orbit_search_code_text(t text) RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT regexp_replace(
               regexp_replace(regexp_replace(t, '([a-z0-9])([A-Z])', '\1 \2', 'g'), '([A-Z])([A-Z][a-z])', '\1 \2', 'g'),
               '[[:punct:]]+', ' ', 'g')
$$;
-- +goose StatementEnd

-- The simple configuration: no stemming and no stop words, because a piece of
-- an identifier is not English (store.Search drops English stop words from the
-- query it matches this with).
ALTER TABLE search_docs
    ADD COLUMN code_tsv tsvector GENERATED ALWAYS AS
        (setweight(to_tsvector('simple', orbit_search_code_text(coalesce(title, ''))), 'A')
         || setweight(to_tsvector('simple', orbit_search_code_text(coalesce(tool, ''))), 'B')
         || to_tsvector('simple', orbit_search_code_text(body))) STORED,
    -- What a tool call's input names: the repository (an orbit repository
    -- tool's repo, else the <name> of a path under inputs/repos/<name>/) and
    -- the file (file_path, notebook_path or path). NULL for every other kind.
    ADD COLUMN repo text,
    ADD COLUMN path text,
    -- A skill version: the skill's name and version.
    ADD COLUMN skill text,
    ADD COLUMN skill_version integer;
CREATE INDEX search_docs_code_tsv ON search_docs USING gin (code_tsv);
CREATE INDEX search_docs_skill ON search_docs (skill) WHERE skill IS NOT NULL;

ALTER TABLE search_docs DROP CONSTRAINT search_docs_kind_check;
ALTER TABLE search_docs ADD CONSTRAINT search_docs_kind_check
    CHECK (kind IN ('message', 'question', 'report', 'error', 'narration', 'tool_call', 'memory', 'memory_entry', 'skill', 'session'));
-- A skill, like a profile or topic memory entry, belongs to no one agent.
ALTER TABLE search_docs DROP CONSTRAINT search_docs_check;
ALTER TABLE search_docs ADD CONSTRAINT search_docs_agent_check
    CHECK ((agent IS NULL) = ((kind = 'memory_entry' AND scope <> 'agent') OR kind IN ('skill', 'session')));

-- orbit_search_tool_target reads the repository and file a tool call's input
-- names. An input that is not a JSON object (a codex command, an input clipped
-- past its end) names neither.
-- +goose StatementBegin
CREATE FUNCTION orbit_search_tool_target(body text, OUT repo text, OUT path text) LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    j jsonb;
BEGIN
    BEGIN
        j := body::jsonb;
    EXCEPTION WHEN OTHERS THEN
        RETURN;
    END;
    IF jsonb_typeof(j) <> 'object' THEN
        RETURN;
    END IF;
    path := NULLIF(coalesce(j->>'file_path', j->>'notebook_path', j->>'path'), '');
    repo := lower(NULLIF(j->>'repo', ''));
    IF repo IS NULL AND path IS NOT NULL THEN
        repo := lower(substring(path FROM '(?:^|/)inputs/repos/([^/]+)/'));
    END IF;
END;
$$;
-- +goose StatementEnd

-- The event trigger as before, with a tool call's repository and file.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION orbit_search_index_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        INSERT INTO search_docs (ref, kind, agent, conversation_id, run_id, attempt, seq, tool, repo, path, at, body)
        SELECT 'event:' || NEW.run_id || ':' || NEW.attempt || ':' || NEW.seq || ':' || d.n,
               d.kind, r.agent, r.conversation_id, NEW.run_id, NEW.attempt, NEW.seq, d.tool,
               CASE WHEN d.kind = 'tool_call' THEN (orbit_search_tool_target(d.body)).repo END,
               CASE WHEN d.kind = 'tool_call' THEN (orbit_search_tool_target(d.body)).path END,
               NEW.at, orbit_search_clip(d.body)
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

-- A skill version: its name (the title), description and SKILL.md. Search
-- shows a skill's current version only, and not once retired, read at query
-- time as for memory entries.
-- +goose StatementBegin
CREATE FUNCTION orbit_search_index_skill() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        INSERT INTO search_docs (ref, kind, run_id, conversation_id, skill, skill_version, author, title, at, body)
        SELECT 'skill:' || NEW.skill || ':' || NEW.version, 'skill', NEW.source_run, r.conversation_id,
               NEW.skill, NEW.version, NEW.author, NEW.skill, NEW.created_at,
               orbit_search_clip(concat_ws(E'\n\n', NULLIF(NEW.description, ''), NEW.content))
        FROM (SELECT 1) one LEFT JOIN runs r ON r.id = NEW.source_run
        ON CONFLICT (ref) DO NOTHING;
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'search: skill % version %: %', NEW.skill, NEW.version, SQLERRM;
    END;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER skill_versions_search AFTER INSERT ON skill_versions
    FOR EACH ROW EXECUTE FUNCTION orbit_search_index_skill();

-- Every word of every document, for typo repair: lowercase, letters only, 3
-- to 32 of them (a shorter word has too few trigrams to match, a longer one
-- is not a word anyone mistypes). Rows are only ever added; a word whose
-- documents changed stays, and repair checks a candidate by searching it.
CREATE TABLE search_terms (
    term text PRIMARY KEY
);
CREATE INDEX search_terms_trgm ON search_terms USING gin (term gin_trgm_ops);

-- Terms are inserted in lexeme order (unnest of a tsvector is sorted), so two
-- writers inserting the same new words wait on each other in one order.
-- +goose StatementBegin
CREATE FUNCTION orbit_search_index_terms() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    BEGIN
        INSERT INTO search_terms (term)
        SELECT t.lexeme FROM unnest(NEW.code_tsv) t
        WHERE t.lexeme ~ '^[a-z]{3,32}$'
        ON CONFLICT (term) DO NOTHING;
    EXCEPTION WHEN OTHERS THEN
        RAISE WARNING 'search: terms of %: %', NEW.ref, SQLERRM;
    END;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER search_docs_terms AFTER INSERT OR UPDATE OF body ON search_docs
    FOR EACH ROW EXECUTE FUNCTION orbit_search_index_terms();

-- The backfill. The new trigger exists already and CREATE TRIGGER holds off
-- concurrent writes to skill_versions until this commits.
UPDATE search_docs d SET repo = t.repo, path = t.path
FROM search_docs s, orbit_search_tool_target(s.body) t
WHERE s.id = d.id AND d.kind = 'tool_call';

INSERT INTO search_docs (ref, kind, run_id, conversation_id, skill, skill_version, author, title, at, body)
SELECT 'skill:' || v.skill || ':' || v.version, 'skill', v.source_run, r.conversation_id, v.skill, v.version,
       v.author, v.skill, v.created_at, orbit_search_clip(concat_ws(E'\n\n', NULLIF(v.description, ''), v.content))
FROM skill_versions v LEFT JOIN runs r ON r.id = v.source_run
ON CONFLICT (ref) DO NOTHING;

INSERT INTO search_terms (term)
SELECT DISTINCT t.lexeme FROM search_docs d, unnest(d.code_tsv) t
WHERE t.lexeme ~ '^[a-z]{3,32}$'
ON CONFLICT (term) DO NOTHING;

-- +goose Down
DROP TRIGGER search_docs_terms ON search_docs;
DROP FUNCTION orbit_search_index_terms();
DROP TABLE search_terms;
DROP TRIGGER skill_versions_search ON skill_versions;
DROP FUNCTION orbit_search_index_skill();
DELETE FROM search_docs WHERE kind = 'skill';
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION orbit_search_index_event() RETURNS trigger LANGUAGE plpgsql AS $$
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
DROP FUNCTION orbit_search_tool_target(text);
ALTER TABLE search_docs DROP CONSTRAINT search_docs_agent_check;
ALTER TABLE search_docs ADD CONSTRAINT search_docs_check
    CHECK ((agent IS NULL) = (kind = 'session' OR (kind = 'memory_entry' AND scope <> 'agent')));
ALTER TABLE search_docs DROP CONSTRAINT search_docs_kind_check;
ALTER TABLE search_docs ADD CONSTRAINT search_docs_kind_check
    CHECK (kind IN ('message', 'question', 'report', 'error', 'narration', 'tool_call', 'memory', 'memory_entry', 'session'));
ALTER TABLE search_docs DROP COLUMN skill_version, DROP COLUMN skill, DROP COLUMN path, DROP COLUMN repo,
    DROP COLUMN code_tsv;
DROP FUNCTION orbit_search_code_text(text);
