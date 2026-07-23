-- +goose Up
-- Slice 1 of "field_map + enterprise_uid" feature (IDENTITY_ARCHITECTURE_ROADMAP).
--
-- Adds enterprise_uid column to employment_identities. Stub write path in
-- ApplyEnterpriseMapping leaves it NULL for now; slice 2 (real provider
-- clients + JWT claims) populates it via the tenant's field_map config.
--
-- The unique index is PARTIAL (WHERE enterprise_uid IS NOT NULL) so multiple
-- stub rows with NULL enterprise_uid coexist; once slice 2 fills the column,
-- the per-tenant uniqueness contract (one enterprise_uid per tenant) is
-- enforced automatically. PG's default NULL-distinctness would technically
-- allow a full unique index too, but the partial form is explicit about
-- intent and avoids bloated index entries for the NULL stub rows.

-- +goose StatementBegin
ALTER TABLE employment_identities
    ADD COLUMN IF NOT EXISTS enterprise_uid VARCHAR(191);

CREATE UNIQUE INDEX IF NOT EXISTS uq_employment_identities_tenant_enterprise_uid
    ON employment_identities (tenant_id, enterprise_uid)
    WHERE enterprise_uid IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS uq_employment_identities_tenant_enterprise_uid;
ALTER TABLE employment_identities DROP COLUMN IF EXISTS enterprise_uid;
-- +goose StatementEnd

