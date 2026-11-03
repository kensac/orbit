-- +goose Up

-- The stall check (docs/ARCHITECTURE.md, Delegation): set on a batch's open
-- tasks when none of them can move (each waits on a dependency that failed,
-- or on a person's decision pending past defaults.decision_stall) and the
-- parent was woken for it, so it is woken once; cleared when the task can
-- move again.
ALTER TABLE tasks ADD COLUMN stalled_at timestamptz;

-- +goose Down
ALTER TABLE tasks DROP COLUMN stalled_at;
