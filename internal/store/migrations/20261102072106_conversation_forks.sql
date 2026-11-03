-- +goose Up

-- A conversation forked from another at one of its messages
-- (POST /conversations/{id}/fork, store.ForkConversation). The fork holds
-- the thread as it stood once that message was answered: fork_cutoff is
-- the answering run's finish (or the message's time when none answered it),
-- and the fork's runs see the parent's thread up to then (store.Conversation).
-- fork_run is the run whose session the fork's first run continues, at
-- fork_resume_at (the last assistant message that run wrote, Claude Code's
-- --resume-session-at), so a session that went on in a live process is cut
-- where the fork was made.
ALTER TABLE conversations
    ADD COLUMN forked_from uuid REFERENCES conversations (id),
    ADD COLUMN forked_at_message uuid REFERENCES messages (id),
    ADD COLUMN fork_cutoff timestamptz,
    ADD COLUMN fork_run uuid REFERENCES runs (id),
    ADD COLUMN fork_resume_at text,
    ADD CONSTRAINT conversations_fork_check
        CHECK ((forked_from IS NULL) = (forked_at_message IS NULL) AND (forked_from IS NULL) = (fork_cutoff IS NULL)
               AND (fork_run IS NULL OR forked_from IS NOT NULL));

-- +goose Down
ALTER TABLE conversations DROP CONSTRAINT conversations_fork_check,
    DROP COLUMN fork_resume_at, DROP COLUMN fork_run, DROP COLUMN fork_cutoff,
    DROP COLUMN forked_at_message, DROP COLUMN forked_from;
