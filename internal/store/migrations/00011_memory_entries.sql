-- +goose Up

-- Memory entries: durable facts, one per entry, each typed and kept in every version. Beside an agent's own memory document (memory_versions), these are what its person taught it and what it learned, shared at three scopes: the person's profile (every agent), a topic (every agent that reads it, such as finance), and one agent.
CREATE TABLE memory_entries (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    scope            text NOT NULL CHECK (scope IN ('profile', 'topic', 'agent')),
    -- The topic or agent name; '' for the profile.
    owner            text NOT NULL DEFAULT '',
    -- A slug naming the entry within its scope, stable across versions:
    -- an import matches on it, and the rendered index cites it.
    key              text NOT NULL,
    -- The newest version; an edit names the version it read, and a write that finds another is refused (store.ErrConflict).
    current_version  integer NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope, owner, key),
    CHECK ((scope = 'profile') = (owner = ''))
);

-- Every version of every entry, newest the one in force. Nothing is deleted: retiring an entry, or superseding it with another, is a version with status retired.
CREATE TABLE memory_entry_versions (
    entry_id             uuid NOT NULL REFERENCES memory_entries (id),
    version              integer NOT NULL,
    status               text NOT NULL CHECK (status IN ('active', 'retired')),
    -- Claude Code's memory types: who the person is (user), how they want work done (feedback), facts about their world and work (project), where to find things (reference).
    type                 text NOT NULL CHECK (type IN ('user', 'feedback', 'project', 'reference')),
    title                text NOT NULL,
    -- One line for the index every run is shown.
    description          text NOT NULL DEFAULT '',
    body                 text NOT NULL,
    why                  text NOT NULL DEFAULT '',
    how_to_apply         text NOT NULL DEFAULT '',
    superseded_by        uuid REFERENCES memory_entries (id),
    -- Who wrote this version: a person (API), a learning run, an import.
    source               text NOT NULL CHECK (source IN ('person', 'learned', 'import')),
    source_run           uuid REFERENCES runs (id),
    source_conversation  uuid REFERENCES conversations (id),
    author               text,
    -- Why this version was written, in a sentence.
    note                 text NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (entry_id, version),
    CHECK (superseded_by IS NULL OR status = 'retired'),
    CHECK (superseded_by IS DISTINCT FROM entry_id)
);

-- Context attached to an agent through the API, beside what its spec names: a topic whose entries it reads, a skill it runs with. A detach ends the row rather than deleting it.
CREATE TABLE agent_attachments (
    id           bigserial PRIMARY KEY,
    agent        text NOT NULL REFERENCES agents (name),
    kind         text NOT NULL CHECK (kind IN ('topic', 'skill')),
    name         text NOT NULL,
    attached_by  text,
    attached_at  timestamptz NOT NULL DEFAULT now(),
    detached_by  text,
    detached_at  timestamptz
);
CREATE UNIQUE INDEX agent_attachments_current ON agent_attachments (agent, kind, name) WHERE detached_at IS NULL;

-- +goose Down
DROP TABLE agent_attachments;
DROP TABLE memory_entry_versions;
DROP TABLE memory_entries;
