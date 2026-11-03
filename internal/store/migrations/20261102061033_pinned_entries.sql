-- +goose Up

-- A pinned feedback entry is repeated to every turn of its readers' runs, beside the index, so a rule does not fade as a long session compacts what it was first shown. Versioned like every other field: pinning or unpinning is a version.
ALTER TABLE memory_entry_versions ADD COLUMN pinned boolean NOT NULL DEFAULT false;
ALTER TABLE memory_entry_versions ADD CONSTRAINT memory_entry_versions_pinned_feedback CHECK (NOT pinned OR type = 'feedback');

-- +goose Down
ALTER TABLE memory_entry_versions DROP COLUMN pinned;
