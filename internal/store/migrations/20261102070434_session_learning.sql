-- +goose Up

-- Learning runs over imported sessions: a learning run (trigger learn)
-- reads one imported session's lines after the previous succeeded learning
-- run of it through a point (its generation and line count then), instead
-- of a conversation. Each point is learned once.
ALTER TABLE runs
    ADD COLUMN learns_session uuid REFERENCES external_sessions (id),
    ADD COLUMN learn_generation integer,
    ADD COLUMN learn_lines integer;
ALTER TABLE runs DROP CONSTRAINT runs_learn_shape;
ALTER TABLE runs ADD CONSTRAINT runs_learn_shape CHECK (
    (trigger = 'learn') = ((learns_from IS NOT NULL AND learn_through IS NOT NULL)
                           OR (learns_session IS NOT NULL AND learn_generation IS NOT NULL AND learn_lines IS NOT NULL))
    AND NOT (learns_from IS NOT NULL AND learns_session IS NOT NULL));
CREATE UNIQUE INDEX runs_one_session_learning ON runs (learns_session, learn_generation, learn_lines)
    WHERE trigger = 'learn' AND learns_session IS NOT NULL;

-- +goose Down
DROP INDEX runs_one_session_learning;
ALTER TABLE runs DROP CONSTRAINT runs_learn_shape;
ALTER TABLE runs ADD CONSTRAINT runs_learn_shape CHECK ((trigger = 'learn') = (learns_from IS NOT NULL AND learn_through IS NOT NULL));
ALTER TABLE runs DROP COLUMN learn_lines, DROP COLUMN learn_generation, DROP COLUMN learns_session;
