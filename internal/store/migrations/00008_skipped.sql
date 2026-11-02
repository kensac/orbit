-- +goose Up

-- skipped: a run that ended without a backend call, because a gate (code,
-- not a model) decided there was nothing to judge. gate names it, so a run
-- that judged nothing is told apart from one that did. A skipped run's
-- attempt ends skipped too.
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled', 'skipped'));
ALTER TABLE runs ADD COLUMN gate text;
ALTER TABLE runs ADD CONSTRAINT runs_skipped_names_gate CHECK ((status = 'skipped') = (gate IS NOT NULL));
ALTER TABLE run_attempts DROP CONSTRAINT run_attempts_outcome_check;
ALTER TABLE run_attempts ADD CONSTRAINT run_attempts_outcome_check
    CHECK (outcome IN ('succeeded', 'failed', 'handed_back', 'lost', 'skipped'));

-- +goose Down
ALTER TABLE run_attempts DROP CONSTRAINT run_attempts_outcome_check;
ALTER TABLE run_attempts ADD CONSTRAINT run_attempts_outcome_check
    CHECK (outcome IN ('succeeded', 'failed', 'handed_back', 'lost'));
ALTER TABLE runs DROP CONSTRAINT runs_skipped_names_gate;
ALTER TABLE runs DROP COLUMN gate;
ALTER TABLE runs DROP CONSTRAINT runs_status_check;
ALTER TABLE runs ADD CONSTRAINT runs_status_check
    CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled'));
