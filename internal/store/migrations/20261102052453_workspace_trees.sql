-- +goose Up

-- A workspace (docs/workspace.md): one tree of files a person and an agent
-- share, behind the workspace API. On the snapshot backend it is one
-- conversation's (its content is the conversation's newest row of
-- workspaces, the snapshots); a box is one agent's (scope agent). The id is
-- what the API and conversations call workspace_id.
CREATE TABLE workspace_trees (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    scope           text NOT NULL CHECK (scope IN ('conversation', 'agent')),
    agent           text NOT NULL,
    conversation_id uuid UNIQUE REFERENCES conversations (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT workspace_trees_scope_conversation CHECK ((scope = 'conversation') = (conversation_id IS NOT NULL))
);
CREATE UNIQUE INDEX workspace_trees_one_per_agent ON workspace_trees (agent) WHERE scope = 'agent';

-- Every conversation that already has a snapshot has a workspace.
INSERT INTO workspace_trees (scope, agent, conversation_id)
SELECT 'conversation', c.agent, c.id FROM conversations c
WHERE EXISTS (SELECT 1 FROM workspaces w WHERE w.conversation_id = c.id);

-- The workspace audit (docs/api.md, Workspace, Audit): one row per
-- operation, append-only like the journal. Terminal input is never here.
CREATE TABLE workspace_events (
    id             bigserial PRIMARY KEY,
    workspace_id   uuid NOT NULL REFERENCES workspace_trees (id),
    at             timestamptz NOT NULL DEFAULT now(),
    -- The verified person; NULL for the operator token or a run.
    person         text,
    op             text NOT NULL,
    path           text NOT NULL,
    from_path      text,
    version_before text,
    version_after  text,
    bytes          bigint,
    terminal_id    uuid,
    run_id         uuid REFERENCES runs (id),
    request_id     text,
    -- The snapshot the operation stored (snapshot backend).
    snapshot_id    bigint REFERENCES workspaces (id)
);
CREATE INDEX workspace_events_workspace ON workspace_events (workspace_id, id DESC);

-- +goose Down
DROP TABLE workspace_events;
DROP TABLE workspace_trees;
