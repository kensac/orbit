-- +goose Up

-- A pending CLI permission request (approvals) waits on its person like an action does, so it gets an attention row: kind 'permission', key approval:<id>, open while the approval is pending and unexpired (attention.go). Additive: the kinds CHECK widens and approval_id is a new nullable column.
ALTER TABLE attention ADD COLUMN approval_id uuid REFERENCES approvals (id);
ALTER TABLE attention DROP CONSTRAINT attention_kind_check;
ALTER TABLE attention ADD CONSTRAINT attention_kind_check CHECK (kind IN ('approval', 'question', 'failure', 'decision', 'permission'));

-- +goose Down
ALTER TABLE attention DROP CONSTRAINT attention_kind_check;
ALTER TABLE attention ADD CONSTRAINT attention_kind_check CHECK (kind IN ('approval', 'question', 'failure', 'decision'));
ALTER TABLE attention DROP COLUMN approval_id;
