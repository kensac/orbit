-- +goose Up

-- The skills library: Claude Code skills (a SKILL.md and optional files) kept in orbit, every version kept, attached to agents by spec (`skills`) or the API (agent_attachments, kind skill). A run is given its agent's skills at their current versions (engine/skills.go).
CREATE TABLE skills (
    name             text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    -- The newest version; a write names the version it read, and one that finds another is refused (store.ErrConflict).
    current_version  integer NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE skill_versions (
    skill        text NOT NULL REFERENCES skills (name),
    version      integer NOT NULL,
    status       text NOT NULL CHECK (status IN ('active', 'retired')),
    -- From SKILL.md's frontmatter: what Claude Code shows the model to decide when to use it.
    description  text NOT NULL,
    -- SKILL.md, frontmatter included.
    content      text NOT NULL,
    -- Over content and every file, so a run's copy is reconciled by it and an unchanged write is no version.
    sha256       text NOT NULL,
    source       text NOT NULL CHECK (source IN ('person', 'agent', 'import')),
    source_run   uuid REFERENCES runs (id),
    author       text,
    note         text NOT NULL DEFAULT '',
    restored_from integer,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (skill, version)
);

-- A version's other files (references, scripts, assets), by path under the skill's directory.
CREATE TABLE skill_files (
    skill    text NOT NULL,
    version  integer NOT NULL,
    path     text NOT NULL,
    content  bytea NOT NULL,
    PRIMARY KEY (skill, version, path),
    FOREIGN KEY (skill, version) REFERENCES skill_versions (skill, version)
);

-- +goose Down
DROP TABLE skill_files;
DROP TABLE skill_versions;
DROP TABLE skills;
