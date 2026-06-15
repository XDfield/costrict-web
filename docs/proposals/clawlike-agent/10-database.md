# 10. 数据库设计

所有新增表通过 GORM AutoMigrate 创建。使用 costrict-web 现有的 PostgreSQL 实例。

## agent_personas 表

```sql
CREATE TABLE agent_personas (
    id                UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id           VARCHAR(255) NOT NULL,
    name              VARCHAR(255) NOT NULL,
    soul_content      TEXT         NOT NULL,
    identity_content  TEXT,
    user_context      TEXT,
    is_default        BOOLEAN      DEFAULT FALSE,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uk_persona_user_name UNIQUE (user_id, name)
);

CREATE INDEX idx_agent_personas_user ON agent_personas(user_id);
```

## agent_providers 表

```sql
CREATE TABLE agent_providers (
    id                BIGSERIAL    PRIMARY KEY,
    user_id           VARCHAR(255) NOT NULL,
    name              VARCHAR(255) NOT NULL,
    provider_type     VARCHAR(50)  NOT NULL,
    api_key_encrypted TEXT,
    base_url          TEXT,
    model_name        VARCHAR(255) NOT NULL,
    models            TEXT,                       -- JSON array of model configs
    is_default        BOOLEAN      DEFAULT FALSE,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uk_provider_user_name UNIQUE (user_id, name)
);

CREATE INDEX idx_agent_providers_user ON agent_providers(user_id);
```

## agent_workspace_tasks 表

记录 workspace 委托任务的生命周期。借鉴 agent-runtime 提案的 TaskRecord 设计，包含完整状态机和交付管理。

```sql
CREATE TABLE agent_workspace_tasks (
    id               UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id          VARCHAR(64)  NOT NULL UNIQUE,
    user_id          VARCHAR(255) NOT NULL,
    workspace_id     VARCHAR(255) NOT NULL,
    device_id        VARCHAR(255) NOT NULL,
    directory_path   TEXT,
    task             TEXT         NOT NULL,
    skill            VARCHAR(255),
    agent_session_id VARCHAR(255),                -- ClawAgent 会话 ID（用于 announce 回传）
    conversation_id  VARCHAR(255),                -- cs-cloud 设备端会话 ID
    status           VARCHAR(20)  NOT NULL DEFAULT 'queued',  -- 见状态机
    delivery_status  VARCHAR(20)  NOT NULL DEFAULT 'pending', -- 见交付状态机
    progress_summary TEXT,                        -- 中间进度摘要（SSE 事件流更新）
    output           TEXT,                        -- 最终输出
    error            TEXT,
    started_at       TIMESTAMPTZ,
    completed_at     TIMESTAMPTZ,
    last_event_at    TIMESTAMPTZ,                 -- 最后收到设备事件的时间（心跳/存活判定）
    created_at       TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_workspace_tasks_user           ON agent_workspace_tasks(user_id);
CREATE INDEX idx_workspace_tasks_workspace      ON agent_workspace_tasks(workspace_id);
CREATE INDEX idx_workspace_tasks_device         ON agent_workspace_tasks(device_id);
CREATE INDEX idx_workspace_tasks_status         ON agent_workspace_tasks(status);
CREATE INDEX idx_workspace_tasks_conversation   ON agent_workspace_tasks(conversation_id);
CREATE INDEX idx_workspace_tasks_agent_session  ON agent_workspace_tasks(agent_session_id);
```

### 任务状态机

```
                  ┌─────────────────────────────┐
                  │                             ▼
  ──► queued ──► running ──► succeeded
                  │   │
                  │   ├──► failed
                  │   │
                  │   └──► timed_out
                  │
                  └──────────► cancelled

  任何非终态 ──────────────► lost (服务重启后恢复标记)
```

| 状态 | 说明 |
|------|------|
| `queued` | 已入队，等待发送到设备端 |
| `running` | 已发送到设备端，正在执行（通过 SSE 监听中） |
| `succeeded` | 设备端返回 `session.idle`，任务成功完成 |
| `failed` | 设备端返回 `session.error`，任务失败 |
| `timed_out` | 超过 timeout 未收到完成事件 |
| `cancelled` | 用户或 Agent 主动中止（`POST /conversations/{id}/abort`） |
| `lost` | 服务重启后发现非终态任务，标记为 lost（崩溃恢复） |

### 交付状态机

```
  pending ──► delivered        (结果已回传给 Agent session，announce 成功)
           ├─► failed           (announce 重试超限，回传失败)
           └─► not_applicable   (阻塞模式同步返回，无需异步回传)
```

| 状态 | 说明 |
|------|------|
| `pending` | 结果待交付（等待 announce 回传到 Agent） |
| `delivered` | 已通过 `runner.Run()` 注入到 Agent session |
| `failed` | 交付失败（announce 指数退避重试 5 次后放弃） |
| `not_applicable` | 阻塞模式（blocking=true），同步返回结果，无需异步回传 |

## clawagent_memories 表（自动创建）

此表由 trpc-agent-go 的 `memory/postgres` 后端**自动创建**，无需手动 migration。

初始化时通过 `memory.NewPostgresMemory()` 配置表名后，框架会执行 `CREATE TABLE IF NOT EXISTS`：

```sql
-- 由 trpc-agent-go 自动执行（表名可配，默认 clawagent_memories）
CREATE TABLE IF NOT EXISTS clawagent_memories (
    memory_id     TEXT        PRIMARY KEY,
    app_name      TEXT        NOT NULL,
    user_id       TEXT        NOT NULL,
    memory_data   JSONB       NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ                          -- 软删除（可配）
);

CREATE INDEX IF NOT EXISTS idx_clawagent_memories_user      ON clawagent_memories(user_id);
CREATE INDEX IF NOT EXISTS idx_clawagent_memories_app_user  ON clawagent_memories(app_name, user_id);
CREATE INDEX IF NOT EXISTS idx_clawagent_memories_deleted   ON clawagent_memories(deleted_at);
```

**配置项**（在 `setup.go` 中）：

```go
memOpts := []memory.PostgresOption{
    memory.WithPostgresClientDSN(cfg.DB.DSN),
    memory.WithTableName("clawagent_memories"),
    memory.WithSoftDelete(true),
    memory.WithMinSearchScore(0.1),   // TF-IDF 最小匹配分数
    memory.WithMaxResults(10),
    memory.WithExtractor(memory.DefaultExtractor()), // 自动记忆提取
}
```

> **注意**：不需要 pgvector 扩展。`memory/postgres` 后端使用 PostgreSQL 内置的全文检索 + TF-IDF 关键词匹配，无需向量数据库。

## clawagent_sessions 表（自动创建）

此表由 trpc-agent-go 的 `session/postgres` 后端**自动创建**，用于会话上下文持久化。

```sql
-- 由 trpc-agent-go 自动执行（表名可配）
CREATE TABLE IF NOT EXISTS clawagent_sessions (
    session_id   TEXT        PRIMARY KEY,
    app_name     TEXT        NOT NULL,
    user_id      TEXT        NOT NULL,
    session_data JSONB       NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

## 无状态横向扩展

所有运行时状态均持久化到 PostgreSQL，服务实例可随时重启或横向扩缩：

| 状态类型 | 存储位置 | 说明 |
|----------|----------|------|
| Persona（人格） | `agent_personas` 表 | 用户自定义 Agent 人格，GORM 管理 |
| Provider（模型配置） | `agent_providers` 表 | 用户自配 LLM Provider，GORM 管理 |
| Memory（记忆） | `clawagent_memories` 表 | trpc-agent-go postgres 后端自动管理 |
| Session（会话上下文） | `clawagent_sessions` 表 | trpc-agent-go postgres 后端自动管理 |
| 委托任务历史 | `agent_workspace_tasks` 表 | GORM 管理 |
| Skill 内容 | `capability_items.content` | 复用现有 Capability Hub 数据 |

> **关键**：进程内不持有任何不可恢复的状态。任何实例崩溃后，另一个实例可从 PostgreSQL 恢复全部上下文（Memory、Session、Persona、Provider）。

## Migration 文件

```sql
-- migrations/20260615000000_create_clawagent_tables.up.sql
-- 包含 agent_personas, agent_providers, agent_workspace_tasks 三张表的 DDL
-- clawagent_memories 和 clawagent_sessions 由 trpc-agent-go 运行时自动创建

-- migrations/20260615000000_create_clawagent_tables.down.sql
-- DROP TABLE IF EXISTS agent_personas, agent_providers, agent_workspace_tasks;
-- clawagent_memories 和 clawagent_sessions 由运维手动清理（如需要）
```

通过 goose 管理，与现有 migration 流程一致。
