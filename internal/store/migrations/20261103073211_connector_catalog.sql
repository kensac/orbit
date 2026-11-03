-- +goose Up

-- User-added OAuth connectors are configuration records, not secrets. Their
-- grants remain in connector_tokens, sealed and versioned separately.
CREATE TABLE connector_catalog (
    name        text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9-]{0,62}$'),
    url         text NOT NULL UNIQUE,
    description text NOT NULL DEFAULT '',
    scopes      text[] NOT NULL DEFAULT '{}',
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE connector_catalog;
