-- +goose Up
-- E2.1: Create idp_sources table for per-tenant IdP configuration.
-- This table stores tenant-level identity provider sources (OAuth/OIDC/LDAP)
-- that can be enabled/disabled per tenant, matching MULTI_TENANCY_DESIGN.md §19.1.
--
-- Schema matches the design:
--   - tenant_id: links to tenants table
--   - provider: references provider_mapping providers (github, google, idtrust, etc.)
--   - config: JSON-encoded provider-specific configuration (OAuth endpoints, LDAP host, etc.)
--   - scope: "global" or "tenant-specific" (future; currently always tenant-specific)
--   - enabled: whether this IdP source is active for the tenant
--   - priority: ranking for primary IdP selection (higher = more preferred)
--
-- This enables E2 tenant-level IdP integration, allowing each tenant to have
-- their own set of enabled identity providers with custom configurations.

CREATE TABLE IF NOT EXISTS "idp_sources" (
	"id" BIGSERIAL PRIMARY KEY,
	"tenant_id" VARCHAR(64) NOT NULL,
	"provider" VARCHAR(64) NOT NULL,
	"config" JSONB NOT NULL,
	"scope" VARCHAR(64) NOT NULL DEFAULT 'tenant-specific',
	"enabled" BOOLEAN NOT NULL DEFAULT TRUE,
	"priority" INT NOT NULL DEFAULT 0,
	"created_at" TIMESTAMP(3) NULL,
	"updated_at" TIMESTAMP(3) NULL,
	"created_by" VARCHAR(64) NULL,
	"updated_by" VARCHAR(64) NULL,
	CONSTRAINT "idx_tenant_provider" UNIQUE ("tenant_id", "provider")
);

CREATE INDEX IF NOT EXISTS "idx_tenant_enabled" ON "idp_sources" ("tenant_id", "enabled");
CREATE INDEX IF NOT EXISTS "idx_priority" ON "idp_sources" ("priority");

COMMENT ON TABLE "idp_sources" IS 'Per-tenant identity provider sources (E2.1)';
COMMENT ON COLUMN "idp_sources"."id" IS 'Primary key';
COMMENT ON COLUMN "idp_sources"."tenant_id" IS 'Tenant identifier (t-<slug>)';
COMMENT ON COLUMN "idp_sources"."provider" IS 'Provider name matching provider_mapping key (e.g. github, google, idtrust)';
COMMENT ON COLUMN "idp_sources"."config" IS 'Provider-specific configuration (OAuth URLs, LDAP host, etc.)';
COMMENT ON COLUMN "idp_sources"."scope" IS 'Configuration scope (global or tenant-specific)';
COMMENT ON COLUMN "idp_sources"."enabled" IS 'Whether this IdP source is active for the tenant';
COMMENT ON COLUMN "idp_sources"."priority" IS 'Priority ranking for primary IdP selection (higher wins ties)';
COMMENT ON COLUMN "idp_sources"."created_at" IS 'Record creation timestamp';
COMMENT ON COLUMN "idp_sources"."updated_at" IS 'Record last update timestamp';
COMMENT ON COLUMN "idp_sources"."created_by" IS 'Creator user identifier';
COMMENT ON COLUMN "idp_sources"."updated_by" IS 'Last updater user identifier';

-- +goose Down
DROP INDEX IF EXISTS "idx_priority";
DROP INDEX IF EXISTS "idx_tenant_enabled";
DROP TABLE IF EXISTS "idp_sources";
