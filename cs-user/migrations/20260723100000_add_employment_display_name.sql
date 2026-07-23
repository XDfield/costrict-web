-- +goose Up
-- +goose StatementBegin

-- Adds display_name to employment_identities. Required because enterprise
-- identity (immutable, IdP-synced every login) MUST carry at minimum 工号
-- (employee_number) + 姓名 (display_name) per the two-layer design that
-- separates mutable basic user info from immutable enterprise identity.
--
-- idtrust (and similar Casdoor-brokered IdPs) return display name as a
-- per-provider property (e.g. properties.oauth_Custom_displayName); cs-user's
-- field_map now extracts it into this column on every ApplyEnterpriseMapping
-- call.
--
-- Nullable on purpose: existing rows created before this migration land
-- without a value; the next ApplyEnterpriseMapping (typically the user's
-- next login) backfills them.
ALTER TABLE employment_identities
    ADD COLUMN IF NOT EXISTS display_name VARCHAR(191);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE employment_identities DROP COLUMN IF EXISTS display_name;

-- +goose StatementEnd
