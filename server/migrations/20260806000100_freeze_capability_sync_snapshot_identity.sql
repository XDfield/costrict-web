-- Review finding F-25 (P1): the manifest guard validated generation only on
-- INSERT, and froze a row only once complete = true.
--
-- The gap those two rules leave open: insert a perfectly legal INCOMPLETE
-- manifest at the allocator's current value (say generation 1), then UPDATE
-- that same row to an astronomical generation and set complete = true in the
-- same statement. The INSERT check never re-runs, the complete-freeze only
-- compares against OLD.complete (false), and the allocator row is untouched —
-- so nothing objects. csc applies the invented generation and then permanently
-- rejects every honestly allocated number below it as "not strictly greater":
-- the exact silent paralysis F-5's two triggers were built to prevent, reached
-- through the one path they did not cover.
--
-- The fix makes a snapshot's identity — id, principal_id, generation —
-- immutable on UPDATE regardless of completeness. Identity is assigned at
-- INSERT, where the allocator check binds it to a number that was actually
-- issued; there is no legitimate later write to any of the three:
--   - the build path INSERTs the manifest complete in one transaction and
--     never updates it again except for expires_at (extendExpiry,
--     retireSuperseded);
--   - no Go code path updates generation or principal_id (the only writer of
--     generations is allocateSnapshotGeneration, on the allocator table);
--   - incomplete rows cannot even be produced by the application — they exist
--     only through external interference, which is precisely the writer this
--     guard exists to stop.
--
-- Only the function body changes; the trigger created by 20260805000800 stays
-- bound to it.

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION capability_sync_snapshot_manifest_guard()
RETURNS trigger AS $$
DECLARE
    allocated BIGINT;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT generation INTO allocated
        FROM capability_sync_snapshot_generations
        WHERE principal_id = NEW.principal_id;

        -- The allocation and the manifest insert happen in one transaction, so
        -- this sees the number this transaction just took. A caller that did
        -- not allocate sees NULL or a mismatch and is rejected.
        IF allocated IS NULL OR allocated <> NEW.generation THEN
            RAISE EXCEPTION
                'snapshot generation % was not allocated for principal % (allocator holds %)',
                NEW.generation, NEW.principal_id, COALESCE(allocated::text, 'no row')
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;

    -- F-25: identity is frozen for EVERY row, complete or not. The INSERT
    -- check above is the only place these three are validated, so allowing a
    -- later UPDATE to move them would let an incomplete row smuggle in a
    -- generation the allocator never issued.
    IF NEW.id               IS DISTINCT FROM OLD.id
       OR NEW.principal_id  IS DISTINCT FROM OLD.principal_id
       OR NEW.generation    IS DISTINCT FROM OLD.generation THEN
        RAISE EXCEPTION
            'a capability sync snapshot''s identity (id, principal, generation) is immutable (snapshot %)',
            OLD.id
            USING ERRCODE = 'check_violation';
    END IF;

    -- A complete snapshot is the artifact a client verifies a digest against.
    -- Everything that participates in that verification is frozen; expires_at
    -- is not part of it and may be extended, which is what lets an unchanged
    -- snapshot be re-served under its original generation.
    IF OLD.complete AND (
           NEW.contract_version IS DISTINCT FROM OLD.contract_version
        OR NEW.page_count       IS DISTINCT FROM OLD.page_count
        OR NEW.page_size        IS DISTINCT FROM OLD.page_size
        OR NEW.item_count       IS DISTINCT FROM OLD.item_count
        OR NEW.tombstone_count  IS DISTINCT FROM OLD.tombstone_count
        OR NEW.snapshot_digest  IS DISTINCT FROM OLD.snapshot_digest
        OR NEW.content_digest   IS DISTINCT FROM OLD.content_digest
        OR NEW.generated_at     IS DISTINCT FROM OLD.generated_at
        OR NEW.complete         IS DISTINCT FROM OLD.complete
    ) THEN
        RAISE EXCEPTION
            'a complete capability sync snapshot is immutable except for expires_at (snapshot %)',
            OLD.id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Restores the 20260805000800 body verbatim (identity checked only through the
-- complete-freeze).
CREATE OR REPLACE FUNCTION capability_sync_snapshot_manifest_guard()
RETURNS trigger AS $$
DECLARE
    allocated BIGINT;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT generation INTO allocated
        FROM capability_sync_snapshot_generations
        WHERE principal_id = NEW.principal_id;

        IF allocated IS NULL OR allocated <> NEW.generation THEN
            RAISE EXCEPTION
                'snapshot generation % was not allocated for principal % (allocator holds %)',
                NEW.generation, NEW.principal_id, COALESCE(allocated::text, 'no row')
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;

    IF OLD.complete AND (
           NEW.id               IS DISTINCT FROM OLD.id
        OR NEW.principal_id     IS DISTINCT FROM OLD.principal_id
        OR NEW.generation       IS DISTINCT FROM OLD.generation
        OR NEW.contract_version IS DISTINCT FROM OLD.contract_version
        OR NEW.page_count       IS DISTINCT FROM OLD.page_count
        OR NEW.page_size        IS DISTINCT FROM OLD.page_size
        OR NEW.item_count       IS DISTINCT FROM OLD.item_count
        OR NEW.tombstone_count  IS DISTINCT FROM OLD.tombstone_count
        OR NEW.snapshot_digest  IS DISTINCT FROM OLD.snapshot_digest
        OR NEW.content_digest   IS DISTINCT FROM OLD.content_digest
        OR NEW.generated_at     IS DISTINCT FROM OLD.generated_at
        OR NEW.complete         IS DISTINCT FROM OLD.complete
    ) THEN
        RAISE EXCEPTION
            'a complete capability sync snapshot is immutable except for expires_at (snapshot %)',
            OLD.id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
