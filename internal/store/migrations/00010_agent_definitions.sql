-- +goose Up

-- Agents defined through the API rather than a file (docs/api.md, Defining
-- agents). Every change is a new version; nothing is updated in place, so
-- an agent's whole history, and who made each change, stays readable. The
-- newest version of a name is the definition in force, unless it archived
-- the agent.
CREATE TABLE agent_definitions (
    id          bigserial PRIMARY KEY,
    name        text NOT NULL,
    version     integer NOT NULL CHECK (version >= 1),
    -- The agent as JSON, the same fields as an agents/*.yaml file.
    spec        jsonb NOT NULL,
    enabled     boolean NOT NULL,
    archived    boolean NOT NULL DEFAULT false,
    change      text NOT NULL CHECK (change IN ('create', 'update', 'enable', 'disable', 'archive')),
    -- The verified person who made the change; NULL for the operator token.
    author      text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (name, version)
);

-- +goose Down
DROP TABLE agent_definitions;
