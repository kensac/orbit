-- +goose Up

-- A question may carry a form (forms.Field, checked by code when it is recorded), and a person's reply to one the answers it gave (checked against that form when it is posted). Both are NULL for every other message.
ALTER TABLE messages ADD COLUMN fields jsonb, ADD COLUMN answers jsonb;
ALTER TABLE messages ADD CONSTRAINT messages_fields_on_questions CHECK (fields IS NULL OR direction = 'from_agent');
ALTER TABLE messages ADD CONSTRAINT messages_answers_on_replies CHECK (answers IS NULL OR (direction = 'to_agent' AND reply_to IS NOT NULL));

-- +goose Down
ALTER TABLE messages DROP CONSTRAINT messages_answers_on_replies, DROP CONSTRAINT messages_fields_on_questions;
ALTER TABLE messages DROP COLUMN answers, DROP COLUMN fields;
