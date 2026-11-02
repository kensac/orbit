-- +goose Up

-- Delegation (docs/ARCHITECTURE.md, Delegation): an agent hands a task to
-- another agent, which works on it in a conversation of its own, and is
-- woken when its tasks settle. A run in a task's conversation has trigger
-- task (the task starts, or its parent wrote to it); a run that wakes a
-- parent because its tasks settled has trigger delegation.
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'message', 'continue', 'build', 'learn', 'task', 'delegation'));

-- One task: a brief for one agent, from one delegate action of a parent
-- run. The tasks of one action are a batch, and their dependencies (by
-- key) a DAG inside it.
CREATE TABLE tasks (
    id                     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The delegate action that created it; its batch.
    action_id              uuid NOT NULL REFERENCES actions (id),
    key                    text NOT NULL,
    parent_run_id          uuid NOT NULL REFERENCES runs (id),
    parent_agent           text NOT NULL,
    parent_conversation_id uuid NOT NULL,
    agent                  text NOT NULL,
    -- The task's own conversation, of agent: the brief, the parent's
    -- messages, every run that worked on it.
    conversation_id        uuid NOT NULL UNIQUE,
    brief                  text NOT NULL,
    -- A fresh worker: the agent's spec as a template, without its memory
    -- (nothing read, nothing written).
    fresh                  boolean NOT NULL DEFAULT false,
    -- 1 for a task a top-level conversation delegated, one more per level.
    depth                  integer NOT NULL CHECK (depth >= 1),
    -- A person's decision the task waits on: a task:decision action.
    decision_action_id     uuid REFERENCES actions (id),
    status                 text NOT NULL DEFAULT 'waiting'
                           CHECK (status IN ('waiting', 'blocked', 'working', 'needs_input', 'done', 'failed', 'cancelled')),
    -- {run_id, summary, report} of the run that last finished it; kept
    -- when a message reopens a done task.
    report                 jsonb,
    error                  text,
    -- Set when the parent should hear about this task (it needs input, or
    -- its batch settled); cleared once a parent run has started after it.
    report_due_at          timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    finished_at            timestamptz,
    UNIQUE (action_id, key),
    FOREIGN KEY (conversation_id, agent) REFERENCES conversations (id, agent),
    FOREIGN KEY (parent_conversation_id, parent_agent) REFERENCES conversations (id, agent),
    CHECK (status <> 'done' OR report IS NOT NULL),
    CHECK ((status IN ('done', 'failed', 'cancelled')) = (finished_at IS NOT NULL))
);
CREATE INDEX tasks_open ON tasks (created_at) WHERE status NOT IN ('done', 'failed', 'cancelled');
CREATE INDEX tasks_parent ON tasks (parent_conversation_id, created_at);
CREATE INDEX tasks_parent_run ON tasks (parent_run_id);
CREATE INDEX tasks_report_due ON tasks (report_due_at) WHERE report_due_at IS NOT NULL;

CREATE TABLE task_deps (
    task_id    uuid NOT NULL REFERENCES tasks (id),
    depends_on uuid NOT NULL REFERENCES tasks (id),
    PRIMARY KEY (task_id, depends_on),
    CHECK (task_id <> depends_on)
);

-- One wake waiting per parent conversation: a queued one reads every
-- task's state when it starts, so a second would only repeat it.
CREATE UNIQUE INDEX runs_one_delegation ON runs (conversation_id) WHERE trigger = 'delegation' AND status = 'queued';

-- +goose Down
DROP INDEX runs_one_delegation;
DROP TABLE task_deps;
DROP TABLE tasks;
ALTER TABLE runs DROP CONSTRAINT runs_trigger_check;
ALTER TABLE runs ADD CONSTRAINT runs_trigger_check
    CHECK (trigger IN ('schedule', 'manual', 'message', 'continue', 'build', 'learn'));
