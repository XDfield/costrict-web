-- +goose Up
DROP TABLE IF EXISTS multica_releases;

CREATE TABLE multica_sources (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	name VARCHAR(64) NOT NULL UNIQUE,
	repo_url VARCHAR(256) NOT NULL,
	provider VARCHAR(32) NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO multica_sources (name, repo_url, provider)
VALUES ('default', 'https://gitee.com/linkai0924/multica', 'gitee');

-- +goose Down
DROP TABLE IF EXISTS multica_sources;
