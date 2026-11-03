-- +goose Up

-- What each snapshot changed against the one before it in its
-- conversation (docs/api.md, Workspace: the change stream and history):
-- one row per path, computed by code from the two stored snapshots, which
-- are kept, so these rows can always be computed again. A snapshot is
-- indexed once (workspace_change_sets), its rows in the same transaction.
CREATE TABLE workspace_change_sets (
    snapshot_id     bigint PRIMARY KEY REFERENCES workspaces (id),
    conversation_id uuid NOT NULL REFERENCES conversations (id),
    changes         integer NOT NULL,
    indexed_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE workspace_changes (
    snapshot_id     bigint NOT NULL REFERENCES workspace_change_sets (snapshot_id),
    seq             integer NOT NULL CHECK (seq > 0),
    conversation_id uuid NOT NULL REFERENCES conversations (id),
    path            text NOT NULL,
    op              text NOT NULL CHECK (op IN ('create', 'modify', 'trash', 'restore', 'rename')),
    kind            text NOT NULL CHECK (kind IN ('file', 'dir', 'symlink')),
    from_path       text,
    -- A file's version after the change; NULL for a trash and for
    -- directories and symlinks.
    version         text,
    PRIMARY KEY (snapshot_id, seq),
    CHECK ((op = 'rename') = (from_path IS NOT NULL))
);
-- A path's history, newest first.
CREATE INDEX workspace_changes_path ON workspace_changes (conversation_id, path, snapshot_id DESC);

-- A run's changes summarized (docs/api.md, A reply's changes): the
-- snapshots before and after a finished run, and the line counts between
-- them. Both snapshots are fixed once the run is final, so the summary is
-- computed once.
CREATE TABLE run_changes (
    run_id             uuid PRIMARY KEY REFERENCES runs (id),
    before_snapshot_id bigint REFERENCES workspaces (id),
    after_snapshot_id  bigint REFERENCES workspaces (id),
    files              integer NOT NULL,
    additions          integer NOT NULL,
    deletions          integer NOT NULL,
    computed_at        timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE run_changes;
DROP TABLE workspace_changes;
DROP TABLE workspace_change_sets;
