-- +goose Up

-- A revise build applies a person's review comments on top of the PR's
-- head, as a fix applies the failure report.
ALTER TABLE ticket_builds DROP CONSTRAINT ticket_builds_purpose_check;
ALTER TABLE ticket_builds ADD CONSTRAINT ticket_builds_purpose_check CHECK (purpose IN ('build', 'fix', 'revise'));

-- Line comments a person wrote on a ticket's PR diff (docs/tickets.md,
-- Review comments). A comment is a draft until a review submits it; the
-- message that carried it is message_id. An edit is a new row that
-- supersedes the old one, and a withdrawn draft keeps its row.
CREATE TABLE ticket_review_comments (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    ticket_id     uuid NOT NULL REFERENCES tickets (id),
    path          text NOT NULL CHECK (path <> ''),
    -- Lines of the file at head_sha, inclusive; 0 and 0 is the whole file.
    start_line    integer NOT NULL CHECK (start_line >= 0),
    end_line      integer NOT NULL CHECK (end_line >= start_line),
    body          text NOT NULL CHECK (body <> ''),
    author        text,
    head_sha      text NOT NULL,
    supersedes    uuid REFERENCES ticket_review_comments (id),
    withdrawn_at  timestamptz,
    message_id    uuid REFERENCES messages (id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    CHECK ((start_line = 0) = (end_line = 0)),
    CHECK (withdrawn_at IS NULL OR message_id IS NULL)
);
CREATE INDEX ticket_review_comments_ticket ON ticket_review_comments (ticket_id, created_at);
-- A version is edited at most once: two edits of it would fork the comment.
CREATE UNIQUE INDEX ticket_review_comments_one_edit ON ticket_review_comments (supersedes) WHERE supersedes IS NOT NULL;

-- Which comments each build run was given. A comment is delivered once a
-- run that was given it succeeded; until then every later build is given
-- it again, so none is lost and none is applied twice.
CREATE TABLE ticket_build_comments (
    run_id      uuid NOT NULL REFERENCES ticket_builds (run_id),
    comment_id  uuid NOT NULL REFERENCES ticket_review_comments (id),
    PRIMARY KEY (run_id, comment_id)
);
CREATE INDEX ticket_build_comments_comment ON ticket_build_comments (comment_id);

-- +goose Down
DROP TABLE ticket_build_comments;
DROP TABLE ticket_review_comments;
ALTER TABLE ticket_builds DROP CONSTRAINT ticket_builds_purpose_check;
ALTER TABLE ticket_builds ADD CONSTRAINT ticket_builds_purpose_check CHECK (purpose IN ('build', 'fix'));
