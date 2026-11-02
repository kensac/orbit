-- +goose Up

-- A person's write to a workspace stores a snapshot of its own (docs/api.md,
-- Workspace, Files): no run wrote it, so run_id and attempt are NULL and
-- source says who did; author is the verified person (NULL for the
-- operator token).
ALTER TABLE workspaces ALTER COLUMN run_id DROP NOT NULL;
ALTER TABLE workspaces ALTER COLUMN attempt DROP NOT NULL;
ALTER TABLE workspaces ADD COLUMN source text NOT NULL DEFAULT 'run' CHECK (source IN ('run', 'person'));
ALTER TABLE workspaces ADD COLUMN author text;
ALTER TABLE workspaces ADD CONSTRAINT workspaces_run_wrote_it CHECK ((source = 'run') = (run_id IS NOT NULL AND attempt IS NOT NULL));

-- The trash: a path a person moved out of the tree. Its content is the
-- snapshot it was in (snapshot_id), which is kept like every snapshot, so
-- nothing is deleted and every item can be restored. There is no empty.
CREATE TABLE workspace_trash (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspace_trees (id),
    path         text NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('file', 'dir', 'symlink')),
    bytes        bigint NOT NULL,
    snapshot_id  bigint NOT NULL REFERENCES workspaces (id),
    trashed_at   timestamptz NOT NULL DEFAULT now(),
    trashed_by   text,
    -- The most recent restore; an item may be restored again.
    restored_at  timestamptz,
    restored_by  text
);
CREATE INDEX workspace_trash_workspace ON workspace_trash (workspace_id, trashed_at DESC, id DESC);

-- A signed upload URL uploads once: its nonce is recorded with the
-- upload's event.
ALTER TABLE workspace_events ADD COLUMN nonce text UNIQUE;

-- +goose Down
ALTER TABLE workspace_events DROP COLUMN nonce;
DROP TABLE workspace_trash;
ALTER TABLE workspaces DROP CONSTRAINT workspaces_run_wrote_it;
ALTER TABLE workspaces DROP COLUMN author;
ALTER TABLE workspaces DROP COLUMN source;
