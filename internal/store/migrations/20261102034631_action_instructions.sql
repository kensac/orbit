-- +goose Up

-- The person whose message an action answers: a chat with its person is
-- instructions, as typing into Claude Code is, so an action they asked
-- for executes without a second approval, and the row says who asked.
ALTER TABLE actions ADD COLUMN instructed_by text;

-- +goose Down
ALTER TABLE actions DROP COLUMN instructed_by;
