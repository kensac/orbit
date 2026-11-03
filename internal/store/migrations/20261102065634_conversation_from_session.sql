-- +goose Up

-- A conversation started from an imported session (POST /conversations
-- with from_session): its first runs resume that session's transcript,
-- written into session_transcripts under a session id of its own, until
-- one of its runs succeeds with a session it can continue instead.
ALTER TABLE session_transcripts ALTER COLUMN run_id DROP NOT NULL;
ALTER TABLE session_transcripts ALTER COLUMN attempt DROP NOT NULL;
-- A transcript is a run's (run_id, attempt) or built from an imported
-- session (external_session_id), never both.
ALTER TABLE session_transcripts ADD COLUMN external_session_id uuid REFERENCES external_sessions (id);
ALTER TABLE session_transcripts ADD CONSTRAINT session_transcripts_origin
    CHECK ((run_id IS NULL) = (attempt IS NULL) AND (run_id IS NULL) <> (external_session_id IS NULL));

ALTER TABLE conversations ADD COLUMN from_session uuid REFERENCES external_sessions (id);
ALTER TABLE conversations ADD COLUMN seed_session text REFERENCES session_transcripts (session_id);
ALTER TABLE conversations ADD CONSTRAINT conversations_seed CHECK ((from_session IS NULL) = (seed_session IS NULL));

-- +goose Down
ALTER TABLE conversations DROP CONSTRAINT conversations_seed;
ALTER TABLE conversations DROP COLUMN seed_session;
ALTER TABLE conversations DROP COLUMN from_session;
ALTER TABLE session_transcripts DROP CONSTRAINT session_transcripts_origin;
DELETE FROM session_transcripts WHERE external_session_id IS NOT NULL;
ALTER TABLE session_transcripts DROP COLUMN external_session_id;
ALTER TABLE session_transcripts ALTER COLUMN attempt SET NOT NULL;
ALTER TABLE session_transcripts ALTER COLUMN run_id SET NOT NULL;
