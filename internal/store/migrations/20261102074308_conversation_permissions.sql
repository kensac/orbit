-- +goose Up

-- A conversation's permission level (docs/api.md, Conversation permissions): every change is a new version, never an update. The newest unexpired row is in force for the runs its person's messages start; level '' returns the conversation to its agent's own. The level is stored as the person set it and resolved down at run time (spec.Agent.ResolveLevel), so a ceiling lowered later still holds.
CREATE TABLE conversation_permissions (
    conversation_id uuid NOT NULL REFERENCES conversations (id),
    version         integer NOT NULL CHECK (version > 0),
    level           text NOT NULL,
    set_by          text NOT NULL,
    set_at          timestamptz NOT NULL DEFAULT now(),
    expires_at      timestamptz,
    PRIMARY KEY (conversation_id, version)
);

-- +goose Down
DROP TABLE conversation_permissions;
