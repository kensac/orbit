-- +goose Up

-- Line comments a person wrote on a workspace diff or file (docs/api.md,
-- Workspace, Review comments), sent to the conversation's agent as one
-- message, as a ticket's review comments are. A comment is a draft until a
-- review sends it (message_id); an edit is a new row that supersedes the
-- old one, and a withdrawn draft keeps its row. quote is the code the
-- comment is on, read by code from snapshot_id when it was written, so the
-- message quotes what the person saw.
CREATE TABLE workspace_review_comments (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations (id),
    workspace_id    uuid NOT NULL REFERENCES workspace_trees (id),
    path            text NOT NULL CHECK (path <> ''),
    -- Lines of the file on side, inclusive; 0 and 0 is the whole file.
    start_line      integer NOT NULL CHECK (start_line >= 0),
    end_line        integer NOT NULL CHECK (end_line >= start_line),
    side            text NOT NULL CHECK (side IN ('new', 'old')),
    anchor_kind     text NOT NULL CHECK (anchor_kind IN ('run', 'worktree', 'git')),
    anchor_run_id   uuid REFERENCES runs (id),
    -- The snapshot the lines are of; NULL for a whole-file comment on a
    -- side the workspace had no snapshot for.
    snapshot_id     bigint REFERENCES workspaces (id),
    quote           text NOT NULL,
    body            text NOT NULL CHECK (body <> ''),
    author          text,
    supersedes      uuid REFERENCES workspace_review_comments (id),
    withdrawn_at    timestamptz,
    message_id      uuid REFERENCES messages (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK ((start_line = 0) = (end_line = 0)),
    CHECK (withdrawn_at IS NULL OR message_id IS NULL),
    CHECK ((anchor_kind = 'run') = (anchor_run_id IS NOT NULL))
);
CREATE INDEX workspace_review_comments_conversation ON workspace_review_comments (conversation_id, created_at);
-- A version is edited at most once: two edits of it would fork the comment.
CREATE UNIQUE INDEX workspace_review_comments_one_edit ON workspace_review_comments (supersedes) WHERE supersedes IS NOT NULL;

-- +goose Down
DROP TABLE workspace_review_comments;
