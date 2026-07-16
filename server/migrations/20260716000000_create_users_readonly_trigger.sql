-- +goose Up
-- +goose StatementBegin

-- P0-8b safety-net trigger: rejects direct writes to costrict-web's `users`
-- table once the operational cutover is activated. Defense-in-depth on top of
-- the application-layer gate (USER_SERVICE_WRITE_MODE=readonly routes writes
-- to cs-user via RPCWriter); catches anything that bypasses the app — rogue
-- background jobs, admin scripts, other services pointing at the same DB.
--
-- Behavior is gated by the GUC `app.users_readonly_cutover` so the migration
-- can ship ahead of the cutover without blocking step 3's dual-write canary
-- (where the local DB is still authoritative). The GUC defaults to NULL/OFF,
-- in which case the trigger is a no-op. Operators activate it at runbook
-- step 6 via:
--
--     ALTER DATABASE <name> SET app.users_readonly_cutover = 'on';
--
-- New sessions inherit the database-level setting; existing sessions must
-- reconnect. Maintenance writes can bypass within a single session via:
--
--     SET app.users_readonly_cutover = 'off';
--     -- ... write ...
--     SET app.users_readonly_cutover = 'on';
--
-- Rollback per the runbook is `DROP TRIGGER` (the function survives but the
-- trigger stops firing; the matching Down migration drops both).
CREATE OR REPLACE FUNCTION reject_users_write_when_cutover_active()
RETURNS trigger AS $$
DECLARE
    cutover_active text;
BEGIN
    -- Second arg true => return NULL instead of raising when the GUC is unset.
    -- Default behavior is "allow writes" so the trigger is inert until an
    -- operator flips the GUC at runbook step 6.
    cutover_active := current_setting('app.users_readonly_cutover', true);
    IF cutover_active IS NULL OR cutover_active = '' OR lower(cutover_active) = 'off' THEN
        -- BEFORE trigger contract: return OLD for DELETE, NEW otherwise.
        -- Returning NEW for DELETE would silently drop the row.
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'users table is read-only post P0-8b cutover (GUC app.users_readonly_cutover=on). Bypass via SET app.users_readonly_cutover=off in this session for maintenance.';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS users_readonly_before_write ON users;
CREATE TRIGGER users_readonly_before_write
    BEFORE INSERT OR UPDATE OR DELETE ON users
    FOR EACH ROW EXECUTE FUNCTION reject_users_write_when_cutover_active();

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS users_readonly_before_write ON users;
DROP FUNCTION IF EXISTS reject_users_write_when_cutover_active();

-- +goose StatementEnd
