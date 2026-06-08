-- +goose Up
CREATE TABLE multica_releases (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	version VARCHAR(64) NOT NULL,
	platform VARCHAR(64) NOT NULL,
	changelog TEXT,
	download_url VARCHAR(512) NOT NULL,
	sha256 VARCHAR(128) NOT NULL,
	binary_size BIGINT NOT NULL,
	force BOOLEAN NOT NULL DEFAULT FALSE,
	min_client_version VARCHAR(64),
	channel VARCHAR(32) NOT NULL DEFAULT 'stable',
	created_by VARCHAR(128) NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	CONSTRAINT idx_multica_release_version_platform UNIQUE (version, platform)
);

CREATE INDEX idx_multica_release_platform_channel_created
ON multica_releases(platform, channel, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS multica_releases;
