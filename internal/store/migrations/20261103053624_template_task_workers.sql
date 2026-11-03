-- Template-backed task workers keep a pinned agent spec for the task's runs.
ALTER TABLE agents ADD COLUMN task_worker boolean NOT NULL DEFAULT false;

ALTER TABLE tasks ADD COLUMN template_name text;
ALTER TABLE tasks ADD COLUMN template_version integer;
ALTER TABLE tasks ADD CONSTRAINT tasks_template_ref_check
    CHECK ((template_name IS NULL) = (template_version IS NULL));
