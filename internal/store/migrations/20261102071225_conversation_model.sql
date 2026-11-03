-- +goose Up

-- A conversation's model and effort (PATCH /conversations/{id}): NULL is the
-- agent's runner's own. Checked against the runner's kind when set
-- (spec.CheckModel); a run reads them when it starts.
ALTER TABLE conversations ADD COLUMN model text, ADD COLUMN effort text;

-- The effort a run's latest attempt ran at, beside runs.model; NULL is the
-- runner's default.
ALTER TABLE runs ADD COLUMN effort text;

-- +goose Down
ALTER TABLE runs DROP COLUMN effort;
ALTER TABLE conversations DROP COLUMN effort, DROP COLUMN model;
