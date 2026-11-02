-- +goose Up

-- Learning runs: after a conversation goes quiet, a short run reads what was said since the last one and writes durable memory entries. A learning run is in its agent's learning conversation (purpose 'learning', archived, so no list shows it unless asked), never in the conversation it reads, so that conversation's thread, prompt and session are untouched by it.
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'message', 'continue', 'build', 'learn'));
ALTER TABLE runs
    -- The conversation a learning run reads, and the newest of its person's messages the run covers: the next learning run of that conversation reads only what came after.
    ADD COLUMN learns_from uuid REFERENCES conversations (id),
    ADD COLUMN learn_through timestamptz,
    ADD CONSTRAINT runs_learn_shape CHECK ((trigger = 'learn') = (learns_from IS NOT NULL AND learn_through IS NOT NULL));
-- One learning run per conversation and point in it, ever: a failed one is not retried until the person says something new, which bounds what a failing backend can cost.
CREATE UNIQUE INDEX runs_one_learning ON runs (learns_from, learn_through) WHERE trigger = 'learn';

ALTER TABLE conversations ADD COLUMN purpose text CHECK (purpose IN ('learning'));
CREATE UNIQUE INDEX conversations_one_learning ON conversations (agent) WHERE purpose = 'learning';

-- A memory document a person wrote or restored through the API names them; a run's version names its run. restored_from is the version a restore copied.
ALTER TABLE memory_versions ADD COLUMN author text, ADD COLUMN restored_from bigint REFERENCES memory_versions (id);

-- +goose Down
ALTER TABLE memory_versions DROP COLUMN restored_from, DROP COLUMN author;
DROP INDEX conversations_one_learning;
ALTER TABLE conversations DROP COLUMN purpose;
DROP INDEX runs_one_learning;
ALTER TABLE runs DROP CONSTRAINT runs_learn_shape;
ALTER TABLE runs DROP COLUMN learn_through, DROP COLUMN learns_from;
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'message', 'continue', 'build'));
