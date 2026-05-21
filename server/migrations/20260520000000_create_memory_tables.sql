-- +goose Up
-- +goose StatementBegin

-- 记忆文件元信息表
CREATE TABLE IF NOT EXISTS memory_files (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id VARCHAR(191) NOT NULL,
    project_path TEXT NOT NULL,
    work_dir TEXT,
    name VARCHAR(255) NOT NULL,
    slug VARCHAR(255) NOT NULL,
    type VARCHAR(32) NOT NULL,
    description TEXT,
    current_version INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at TIMESTAMP
);

-- 记忆文件唯一约束：同一用户在同一个项目下 slug 唯一
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_file_user_project_slug ON memory_files(user_id, project_path, slug) WHERE deleted_at IS NULL;

-- 常用查询索引
CREATE INDEX IF NOT EXISTS idx_memory_file_user_id ON memory_files(user_id);
CREATE INDEX IF NOT EXISTS idx_memory_file_project_path ON memory_files(project_path);
CREATE INDEX IF NOT EXISTS idx_memory_file_type ON memory_files(type);
CREATE INDEX IF NOT EXISTS idx_memory_file_deleted_at ON memory_files(deleted_at);

-- 记忆版本表
CREATE TABLE IF NOT EXISTS memory_versions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    memory_file_id UUID NOT NULL,
    version INTEGER NOT NULL,
    content_md5 VARCHAR(32),
    storage_key TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- 版本表索引
CREATE INDEX IF NOT EXISTS idx_memory_version_file_id ON memory_versions(memory_file_id);
CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_version_file_version ON memory_versions(memory_file_id, version);

-- 外键约束：级联删除
ALTER TABLE memory_versions
    ADD CONSTRAINT fk_memory_version_file
    FOREIGN KEY (memory_file_id) REFERENCES memory_files(id) ON DELETE CASCADE;

COMMENT ON TABLE memory_files IS '记忆文件元信息表，实体内容存储在 storage backend 中';
COMMENT ON COLUMN memory_files.user_id IS '用户标识（subject_id）';
COMMENT ON COLUMN memory_files.project_path IS '项目路径，如 /Users/linkai/code/csc';
COMMENT ON COLUMN memory_files.work_dir IS '工作目录';
COMMENT ON COLUMN memory_files.slug IS '记忆文件标识，如 user_language';
COMMENT ON COLUMN memory_files.type IS '记忆类型：user | feedback | project | reference';
COMMENT ON COLUMN memory_files.current_version IS '当前版本号';

COMMENT ON TABLE memory_versions IS '记忆版本表，记录每个记忆文件的历史版本';
COMMENT ON COLUMN memory_versions.memory_file_id IS '关联的记忆文件 ID';
COMMENT ON COLUMN memory_versions.version IS '版本号，从 1 开始递增';
COMMENT ON COLUMN memory_versions.content_md5 IS '内容 MD5，用于去重检测';
COMMENT ON COLUMN memory_versions.storage_key IS '文件在 storage backend 中的 key';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS memory_versions;
DROP TABLE IF EXISTS memory_files;

-- +goose StatementEnd
