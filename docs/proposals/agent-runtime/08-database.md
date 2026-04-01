# 8. 数据库设计

新增两张表，通过 GORM AutoMigrate 创建。

## agent_tasks 表

```sql
CREATE TABLE agent_tasks (
    id                BIGSERIAL PRIMARY KEY,
    task_id           VARCHAR(64)  NOT NULL UNIQUE,
    runtime           VARCHAR(20)  NOT NULL DEFAULT 'user',
    requester_session VARCHAR(128) NOT NULL,
    child_session     VARCHAR(128),
    parent_task_id    VARCHAR(64),
    agent_id          VARCHAR(64)  NOT NULL,
    request_id        VARCHAR(64)  NOT NULL,
    user_id           VARCHAR(64)  NOT NULL,
    task              TEXT         NOT NULL,
    status            VARCHAR(20)  NOT NULL DEFAULT 'queued',
    delivery_status   VARCHAR(20)  NOT NULL DEFAULT 'not_applicable',
    progress_summary  TEXT,
    terminal_summary  TEXT,
    error             TEXT,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    started_at        TIMESTAMPTZ,
    ended_at          TIMESTAMPTZ,
    last_event_at     TIMESTAMPTZ
);

CREATE INDEX idx_agent_tasks_user_id    ON agent_tasks(user_id);
CREATE INDEX idx_agent_tasks_status     ON agent_tasks(status);
CREATE INDEX idx_agent_tasks_parent     ON agent_tasks(parent_task_id);
CREATE INDEX idx_agent_tasks_request_id ON agent_tasks(request_id);
CREATE INDEX idx_agent_tasks_created_at ON agent_tasks(created_at);
```

## agent_subagent_runs 表

```sql
CREATE TABLE agent_subagent_runs (
    id                    BIGSERIAL PRIMARY KEY,
    run_id                VARCHAR(64)  NOT NULL UNIQUE,
    child_session_key     VARCHAR(128) NOT NULL,
    requester_session_key VARCHAR(128) NOT NULL,
    task_id               VARCHAR(64)  NOT NULL,
    request_id            VARCHAR(64)  NOT NULL,
    agent_name            VARCHAR(64)  NOT NULL,
    task                  TEXT         NOT NULL,
    outcome               VARCHAR(20),
    end_reason            VARCHAR(20),
    frozen_result_text    TEXT,
    announce_retry_count  INT          NOT NULL DEFAULT 0,
    created_at            TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    started_at            TIMESTAMPTZ,
    ended_at              TIMESTAMPTZ,
    announced_at          TIMESTAMPTZ
);

CREATE INDEX idx_subagent_runs_child     ON agent_subagent_runs(child_session_key);
CREATE INDEX idx_subagent_runs_requester ON agent_subagent_runs(requester_session_key);
CREATE INDEX idx_subagent_runs_task_id   ON agent_subagent_runs(task_id);
```

Migration 文件放置在 `server/migrations/` 下，通过 goose 管理。
