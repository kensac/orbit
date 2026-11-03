-- +goose Up

-- Budget limits can stop scheduled runs. Keep one dismissible, pushable alert
-- per agent and local budget period when a scheduled run first hits a limit.
ALTER TABLE attention DROP CONSTRAINT attention_kind_check;
ALTER TABLE attention ADD CONSTRAINT attention_kind_check
    CHECK (kind IN ('approval', 'question', 'failure', 'decision', 'permission', 'budget'));

-- +goose Down
ALTER TABLE attention DROP CONSTRAINT attention_kind_check;
ALTER TABLE attention ADD CONSTRAINT attention_kind_check
    CHECK (kind IN ('approval', 'question', 'failure', 'decision', 'permission'));
