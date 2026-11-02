-- +goose Up

-- Who changed an agent: author is the person (the verified token's, or the
-- one whose message the agent was answering), run_id the agent's run when
-- an agent made the change itself (agent:define). A revert is a new version
-- with an earlier version's spec.
ALTER TABLE agent_definitions ADD COLUMN run_id uuid REFERENCES runs (id);
ALTER TABLE agent_definitions ADD COLUMN reverts integer;
ALTER TABLE agent_definitions DROP CONSTRAINT agent_definitions_change_check;
ALTER TABLE agent_definitions ADD CONSTRAINT agent_definitions_change_check
    CHECK (change IN ('create', 'update', 'enable', 'disable', 'archive', 'revert'));

-- +goose Down
ALTER TABLE agent_definitions DROP CONSTRAINT agent_definitions_change_check;
ALTER TABLE agent_definitions ADD CONSTRAINT agent_definitions_change_check
    CHECK (change IN ('create', 'update', 'enable', 'disable', 'archive'));
ALTER TABLE agent_definitions DROP COLUMN reverts;
ALTER TABLE agent_definitions DROP COLUMN run_id;
