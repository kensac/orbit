-- +goose Up
-- Imported transcript snapshots are a separate, read-only archive. They are
-- never operational messages, runs, questions or actions.
CREATE TABLE conversation_archives (
    id                    bigserial PRIMARY KEY,
    target_agent          text NOT NULL REFERENCES agents (name),
    source_agent          text NOT NULL,
    source_conversation_id uuid NOT NULL,
    version               integer NOT NULL CHECK (version > 0),
    source_bundle_sha256  text NOT NULL CHECK (source_bundle_sha256 ~ '^[0-9a-f]{64}$'),
    content_sha256        text NOT NULL CHECK (content_sha256 ~ '^[0-9a-f]{64}$'),
    snapshot              jsonb NOT NULL,
    imported_by           text NOT NULL DEFAULT '',
    imported_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (target_agent, source_agent, source_conversation_id, version),
    UNIQUE (target_agent, source_agent, source_conversation_id, content_sha256)
);
CREATE INDEX conversation_archives_latest
    ON conversation_archives (target_agent, source_agent, source_conversation_id, version DESC);

-- Archive rows are historical evidence. A changed import adds a version.
-- +goose StatementBegin
CREATE FUNCTION orbit_conversation_archive_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'conversation archive rows are append-only';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER conversation_archives_immutable
    BEFORE UPDATE OR DELETE ON conversation_archives
    FOR EACH ROW EXECUTE FUNCTION orbit_conversation_archive_immutable();
CREATE TRIGGER conversation_archives_immutable_truncate
    BEFORE TRUNCATE ON conversation_archives
    FOR EACH STATEMENT EXECUTE FUNCTION orbit_conversation_archive_immutable();

-- +goose Down
DROP TRIGGER conversation_archives_immutable ON conversation_archives;
DROP TRIGGER conversation_archives_immutable_truncate ON conversation_archives;
DROP FUNCTION orbit_conversation_archive_immutable();
DROP TABLE conversation_archives;
