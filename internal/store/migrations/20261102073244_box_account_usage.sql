-- +goose Up

-- An account's usage (docs/api.md, Accounts): the newest observation of its
-- rate-limit windows, from the box's status line or a poll, as JSON
-- ({"source", "observed_at", "windows"}), and when it was observed.
ALTER TABLE box_accounts ADD COLUMN usage jsonb;
ALTER TABLE box_accounts ADD COLUMN usage_at timestamptz;

-- +goose Down
ALTER TABLE box_accounts DROP COLUMN usage_at;
ALTER TABLE box_accounts DROP COLUMN usage;
