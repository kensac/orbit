-- +goose Up

-- The templates library: agent specs with typed parameters (internal/templates), every version kept, never deleted; a retired template is offered to no one. Creating an agent from one renders it in code and defines the agent through the same path as POST /agents (engine/templates.go).
CREATE TABLE templates (
    name             text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    -- The newest version; a write names the version it read, and one that finds another is refused (store.ErrConflict).
    current_version  integer NOT NULL DEFAULT 1,
    created_at       timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE template_versions (
    template      text NOT NULL REFERENCES templates (name),
    version       integer NOT NULL,
    status        text NOT NULL CHECK (status IN ('active', 'retired')),
    description   text NOT NULL,
    -- The template document as templates.Canonical writes it: name, description, parameters, spec.
    document      jsonb NOT NULL,
    -- Over the canonical document, so an unchanged write is no version.
    sha256        text NOT NULL,
    source        text NOT NULL CHECK (source IN ('person', 'import')),
    author        text,
    note          text NOT NULL DEFAULT '',
    restored_from integer,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (template, version)
);

-- +goose Down
DROP TABLE template_versions;
DROP TABLE templates;
