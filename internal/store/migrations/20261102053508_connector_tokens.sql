-- +goose Up

-- A connector's OAuth grant (internal/connectors): every sign-in and every
-- refresh is a new version, none is ever updated or deleted, and the newest
-- is the one in use. sealed is the token encrypted under ORBIT_CONNECTOR_KEY,
-- so this table and its backups hold nothing usable without that key.
CREATE TABLE connector_tokens (
    connector   text NOT NULL,
    version     integer NOT NULL CHECK (version > 0),
    sealed      bytea NOT NULL,
    expires_at  timestamptz,
    -- authorize (a person signed in) or refresh (orbit renewed it).
    reason      text NOT NULL CHECK (reason IN ('authorize', 'refresh')),
    created_by  text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (connector, version)
);

-- +goose Down
DROP TABLE connector_tokens;
