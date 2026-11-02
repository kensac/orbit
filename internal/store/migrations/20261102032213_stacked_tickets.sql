-- +goose Up

-- A request stacked on another (docs/tickets.md, Stacked tickets): built on
-- that ticket's branch while its PR is open, its own PR based on it, and
-- moved onto the default branch by code once it merges. Set when filed.
ALTER TABLE tickets ADD COLUMN stack_on uuid REFERENCES tickets (id);
ALTER TABLE tickets ADD CONSTRAINT tickets_stack_not_self CHECK (stack_on <> id);
CREATE INDEX tickets_stacked ON tickets (stack_on) WHERE stack_on IS NOT NULL;

-- +goose Down
DROP INDEX tickets_stacked;
ALTER TABLE tickets DROP CONSTRAINT tickets_stack_not_self;
ALTER TABLE tickets DROP COLUMN stack_on;
