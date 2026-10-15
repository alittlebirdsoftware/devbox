-- 0002_task_ref: a client reference per task. The plane's build scheduler
-- submits each attempt under a deterministic ref (tenant.plan.item.attempt) so
-- a repeated submit, after a crash or a duplicate tick, resolves to the task
-- this host already has instead of filing a second one. Unique when set.
ALTER TABLE tasks ADD COLUMN ref TEXT;
CREATE UNIQUE INDEX idx_tasks_ref ON tasks(ref) WHERE ref IS NOT NULL;
