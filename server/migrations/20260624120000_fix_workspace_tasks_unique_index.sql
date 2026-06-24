-- +goose Up
-- Align the unique constraint on agent_workspace_tasks.task_id with GORM's
-- expected index name (uni_agent_workspace_tasks_task_id).
-- The original migration used inline UNIQUE which PostgreSQL named
-- agent_workspace_tasks_task_id_key; GORM AutoMigrate expects
-- uni_agent_workspace_tasks_task_id and fails with SQLSTATE 42704.

ALTER TABLE agent_workspace_tasks DROP CONSTRAINT IF EXISTS agent_workspace_tasks_task_id_key;
CREATE UNIQUE INDEX IF NOT EXISTS uni_agent_workspace_tasks_task_id ON agent_workspace_tasks(task_id);

-- +goose Down
DROP INDEX IF EXISTS uni_agent_workspace_tasks_task_id;
ALTER TABLE agent_workspace_tasks ADD CONSTRAINT agent_workspace_tasks_task_id_key UNIQUE (task_id);
