-- +goose Up

-- Files a person uploads into a conversation (docs/api.md, Uploads). Each
-- is kept whole, like an artifact; a message names the ones it carries in
-- message_attachments, and the run that reads the message finds them in its
-- workdir under uploads/<name>. name is unique in its conversation, so that
-- path names one file.
CREATE TABLE uploads (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id uuid NOT NULL REFERENCES conversations (id),
    name            text NOT NULL,
    media_type      text NOT NULL,
    bytes           bigint NOT NULL CHECK (bytes >= 0),
    sha256          text NOT NULL,
    content         bytea NOT NULL,
    -- The verified person who uploaded it; NULL for the operator token.
    author          text,
    -- A signed upload URL's nonce: each URL uploads once.
    nonce           text UNIQUE,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (conversation_id, name)
);

CREATE TABLE message_attachments (
    message_id uuid NOT NULL REFERENCES messages (id),
    upload_id  uuid NOT NULL REFERENCES uploads (id),
    position   integer NOT NULL,
    PRIMARY KEY (message_id, upload_id)
);
CREATE INDEX message_attachments_upload ON message_attachments (upload_id);

-- +goose Down
DROP TABLE message_attachments;
DROP TABLE uploads;
