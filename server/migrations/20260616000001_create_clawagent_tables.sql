-- +goose Up
CREATE TABLE IF NOT EXISTS agent_personas (
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

CREATE INDEX IF NOT EXISTS idx_agent_personas_user ON agent_personas(user_id);

CREATE TABLE IF NOT EXISTS agent_providers (
    id                BIGSERIAL    PRIMARY KEY,
    user_id           VARCHAR(255) NOT NULL,
    name              VARCHAR(255) NOT NULL,
    provider_type     VARCHAR(50)  NOT NULL,
    api_key_encrypted TEXT,
    base_url          TEXT,
    model_name        VARCHAR(255) NOT NULL,
    models            TEXT,
    is_default        BOOLEAN      DEFAULT FALSE,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uk_provider_user_name UNIQUE (user_id, name)
);

CREATE INDEX IF NOT EXISTS idx_agent_providers_user ON agent_providers(user_id);

CREATE TABLE IF NOT EXISTS agent_memories (
    user_id    VARCHAR(255) PRIMARY KEY,
    content    TEXT         NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS agent_workspace_tasks (
    id                     UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    task_id                VARCHAR(64)  NOT NULL,
    user_id                VARCHAR(255) NOT NULL,
    workspace_id           VARCHAR(255) NOT NULL,
    device_id              VARCHAR(255) NOT NULL,
    directory_path         TEXT,
    task                   TEXT         NOT NULL,
    skill                  VARCHAR(255),
    agent_session_base_key VARCHAR(255),
    conversation_id        VARCHAR(255),
    status                 VARCHAR(20)  NOT NULL DEFAULT 'queued',
    delivery_status        VARCHAR(20)  NOT NULL DEFAULT 'pending',
    progress_summary       TEXT,
    output                 TEXT,
    error                  TEXT,
    announce_retry_count   INTEGER      DEFAULT 0,
    started_at             TIMESTAMPTZ,
    completed_at           TIMESTAMPTZ,
    last_event_at          TIMESTAMPTZ,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_workspace_tasks_user         ON agent_workspace_tasks(user_id);
CREATE INDEX IF NOT EXISTS idx_workspace_tasks_workspace    ON agent_workspace_tasks(workspace_id);
CREATE INDEX IF NOT EXISTS idx_workspace_tasks_device       ON agent_workspace_tasks(device_id);
CREATE INDEX IF NOT EXISTS idx_workspace_tasks_status       ON agent_workspace_tasks(status);
CREATE INDEX IF NOT EXISTS idx_workspace_tasks_conversation ON agent_workspace_tasks(conversation_id);
CREATE INDEX IF NOT EXISTS idx_workspace_tasks_base_key     ON agent_workspace_tasks(agent_session_base_key);
CREATE UNIQUE INDEX IF NOT EXISTS uni_agent_workspace_tasks_task_id ON agent_workspace_tasks(task_id);

CREATE TABLE IF NOT EXISTS agent_session_meta (
    session_id      VARCHAR(255) PRIMARY KEY,
    user_id         VARCHAR(255) NOT NULL,
    base_key        VARCHAR(255) NOT NULL,
    version         INTEGER      NOT NULL DEFAULT 1,
    reset_type      VARCHAR(20)  NOT NULL,
    last_message_at TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    message_count   INTEGER      NOT NULL DEFAULT 0,
    token_estimate  INTEGER      NOT NULL DEFAULT 0,
    is_archived     BOOLEAN      NOT NULL DEFAULT FALSE,
    archived_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT uk_session_meta_user_base_ver UNIQUE (user_id, base_key, version)
);

CREATE INDEX IF NOT EXISTS idx_session_meta_user_lastmsg ON agent_session_meta(user_id, last_message_at DESC);
CREATE INDEX IF NOT EXISTS idx_session_meta_base_active  ON agent_session_meta(base_key) WHERE is_archived = FALSE;

-- +goose Down
DROP TABLE IF EXISTS agent_session_meta;
DROP TABLE IF EXISTS agent_workspace_tasks;
DROP TABLE IF EXISTS agent_memories;
DROP TABLE IF EXISTS agent_providers;
DROP TABLE IF EXISTS agent_personas;
