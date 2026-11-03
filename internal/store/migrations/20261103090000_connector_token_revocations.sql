-- +goose Up

-- Revocation is an append-only event keyed to the exact grant version.
-- Older sealed tokens remain available for audit, but are never selected again.
CREATE TABLE connector_token_revocations (
    connector   text NOT NULL,
    version     integer NOT NULL CHECK (version > 0),
    revoked_by  text NOT NULL,
    revoked_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (connector, version),
    FOREIGN KEY (connector, version) REFERENCES connector_tokens(connector, version)
);

-- +goose Down
DROP TABLE connector_token_revocations;
