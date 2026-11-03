-- +goose Up

-- task:retry (docs/ARCHITECTURE.md, Delegation): a failed task tried again
-- on the same row. attempt counts its attempts, failures the ones that
-- failed by its agent's work (a run lost with its worker does not count),
-- which the circuit breaker reads: past store.MaxTaskFailures only a
-- person's approval reopens it.
ALTER TABLE tasks ADD COLUMN attempt integer NOT NULL DEFAULT 1 CHECK (attempt >= 1);
ALTER TABLE tasks ADD COLUMN failures integer NOT NULL DEFAULT 0 CHECK (failures >= 0);

-- An attempt a retry ended, as it was then: who worked on it, where, from
-- which brief, and how it failed. The task row holds the current attempt.
CREATE TABLE task_attempts (
    task_id         uuid NOT NULL REFERENCES tasks (id),
    attempt         integer NOT NULL CHECK (attempt >= 1),
    agent           text NOT NULL,
    conversation_id uuid NOT NULL,
    brief           text NOT NULL,
    error           text,
    report          jsonb,
    finished_at     timestamptz,
    -- The task:retry action that ended it.
    retry_action_id uuid NOT NULL REFERENCES actions (id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, attempt),
    FOREIGN KEY (conversation_id, agent) REFERENCES conversations (id, agent)
);

-- +goose Down
DROP TABLE task_attempts;
ALTER TABLE tasks DROP COLUMN failures;
ALTER TABLE tasks DROP COLUMN attempt;
