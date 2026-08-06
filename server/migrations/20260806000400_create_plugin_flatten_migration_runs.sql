-- Durable plan/apply/rollback state for retiring package-derived Plugin child
-- rows (`capability_items.parent_plugin_id`) under the flat capability model.
--
-- These two tables are the migration's source of truth, not a log. The exported
-- JSON artifact is a copy of them, checksummed so an operator can prove the file
-- in their hands is the plan the database is executing. Three properties depend
-- on the plan being durable rather than recomputed:
--
--   1. Compare-and-set. Apply writes a row only while its status and parent link
--      still equal what the plan inventoried. Recomputing the before-state at
--      apply time would compare a value against itself and defeat the check.
--   2. Crash resumption. A run that dies mid-batch resumes by run id and
--      processes only rows still `pending`; nothing is re-applied and nothing is
--      silently dropped.
--   3. Rollback. Restoring the pre-migration state requires knowing it. A
--      rollback also compare-and-sets against the POST-migration state, so a row
--      somebody legitimately changed after the migration is skipped rather than
--      stomped.
--
-- Nothing here hard-deletes: the migration archives and unlinks, and both tables
-- are retained through the compatibility window and operator sign-off.
--
-- THIS FILE IS THE ONLY DEFINITION OF THESE TWO TABLES.
-- `migrate flatten-plugins` reads it from the embedded migration FS and runs
-- the Up block itself when goose has not reached this version yet
-- (cmd/migrate.ensurePluginFlattenTables). It used to keep a hand-copied second
-- copy of this DDL, which had already drifted by two indexes and every column
-- comment before anyone noticed. Do not reintroduce a second copy.
--
-- Because the same text runs against a table that may already exist, every
-- statement below is idempotent AND converging: `CREATE TABLE IF NOT EXISTS`
-- alone would silently keep an older CHECK definition, so the constraints that
-- have changed since the first draft are re-stated with DROP/ADD.

-- +goose Up
-- +goose StatementBegin

CREATE TABLE IF NOT EXISTS plugin_flatten_migration_runs (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Bumped when the plan/artifact shape changes. Apply refuses a run whose
    -- schema_version it does not understand rather than guessing a field's
    -- meaning.
    schema_version  INTEGER     NOT NULL DEFAULT 1,
    -- 'migrate' retires derived rows; 'rollback' restores a specific migrate run.
    mode            TEXT        NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'planned',
    -- Set on a rollback run: the migrate run whose applied rows it reverses.
    source_run_id   UUID        REFERENCES plugin_flatten_migration_runs(id),
    batch_size      INTEGER     NOT NULL DEFAULT 200,
    -- Lowercase SHA-256 hex over the canonical serialization of this run's plan.
    -- Verified before apply and before rollback; a mismatch aborts.
    plan_digest     VARCHAR(64) NOT NULL DEFAULT '',
    -- Per-classification and per-outcome counters, recomputed from the row table
    -- on every transition so the summary can never drift from the rows.
    totals          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_by      TEXT        NOT NULL DEFAULT '',
    notes           TEXT        NOT NULL DEFAULT '',
    planned_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at      TIMESTAMPTZ,
    finished_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT chk_plugin_flatten_runs_mode
        CHECK (mode IN ('migrate', 'rollback')),
    CONSTRAINT chk_plugin_flatten_runs_status
        CHECK (status IN ('planned', 'applying', 'applied', 'partial', 'rolled_back')),
    CONSTRAINT chk_plugin_flatten_runs_digest_format
        CHECK (plan_digest = '' OR plan_digest ~ '^[0-9a-f]{64}$'),
    -- A rollback run must name what it reverses; a migrate run must not.
    CONSTRAINT chk_plugin_flatten_runs_source
        CHECK ((mode = 'rollback') = (source_run_id IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_plugin_flatten_runs_status
    ON plugin_flatten_migration_runs (status, planned_at DESC);
CREATE INDEX IF NOT EXISTS idx_plugin_flatten_runs_source
    ON plugin_flatten_migration_runs (source_run_id)
    WHERE source_run_id IS NOT NULL;

COMMENT ON TABLE plugin_flatten_migration_runs IS 'One plan/apply/rollback run of the plugin flatten migration; never auto-deleted';
COMMENT ON COLUMN plugin_flatten_migration_runs.plan_digest IS 'SHA-256 over the canonical plan; apply and rollback verify it before touching data';
COMMENT ON COLUMN plugin_flatten_migration_runs.status IS 'planned -> applying -> applied|partial; a rollback run ends rolled_back';

-- One row per candidate. Written once at plan time with the full before-state,
-- then updated in place as apply/rollback decides its outcome.
CREATE TABLE IF NOT EXISTS plugin_flatten_migration_rows (
    run_id                  UUID        NOT NULL
        REFERENCES plugin_flatten_migration_runs(id) ON DELETE CASCADE,
    -- Deterministic order within the run (by item id), so batches are stable
    -- across a crash and the artifact is byte-reproducible.
    seq                     BIGINT      NOT NULL,
    item_id                 UUID        NOT NULL,

    -- Identity and provenance, captured at plan time. These are evidence for the
    -- classification, not inputs re-read at apply time.
    item_type               TEXT        NOT NULL DEFAULT '',
    item_slug               TEXT        NOT NULL DEFAULT '',
    registry_id             TEXT        NOT NULL DEFAULT '',
    source_type             TEXT        NOT NULL DEFAULT '',
    content_backend         TEXT        NOT NULL DEFAULT '',
    catalog_entry_dir       TEXT        NOT NULL DEFAULT '',
    bundled_in              TEXT        NOT NULL DEFAULT '',
    source_path             TEXT        NOT NULL DEFAULT '',
    -- The row's recorded source hash: the catalog content SHA for a db-backed
    -- row, the manifest SHA for a git-backed one. Recorded so the artifact says
    -- which upstream revision the classification was made against.
    source_manifest_sha     TEXT        NOT NULL DEFAULT '',
    git_server_id           TEXT        NOT NULL DEFAULT '',
    git_repo_id             BIGINT      NOT NULL DEFAULT 0,
    git_repo_path           TEXT        NOT NULL DEFAULT '',
    git_entry_key           TEXT        NOT NULL DEFAULT '',
    forked_from_item_id     UUID,

    -- Parent identity, and whether it resolved at all. A dangling parent is
    -- evidence, so it is stored rather than inferred later from a failed join.
    parent_item_id          UUID,
    parent_exists           BOOLEAN     NOT NULL DEFAULT false,
    parent_item_type        TEXT        NOT NULL DEFAULT '',
    parent_source_type      TEXT        NOT NULL DEFAULT '',

    -- Consumer impact, for the report. Favorites are preserved: SD-3 forbids
    -- silently moving a favorite to the parent plugin, so the count exists to be
    -- reported, never to authorize a different action.
    favorite_count          INTEGER     NOT NULL DEFAULT 0,
    distribution_count      INTEGER     NOT NULL DEFAULT 0,

    -- Compare-and-set predicate. Apply writes only while the live row still
    -- matches every one of these.
    before_status           TEXT        NOT NULL,
    before_parent_plugin_id UUID,
    -- Intended end state.
    after_status            TEXT        NOT NULL,
    after_parent_plugin_id  UUID,

    classification          TEXT        NOT NULL,
    action                  TEXT        NOT NULL,
    reason                  TEXT        NOT NULL DEFAULT '',
    conflict                TEXT        NOT NULL DEFAULT '',
    row_state               TEXT        NOT NULL DEFAULT 'pending',
    applied_at              TIMESTAMPTZ,

    PRIMARY KEY (run_id, item_id),
    CONSTRAINT chk_plugin_flatten_rows_classification
        CHECK (classification IN (
            'derived_catalog', 'derived_archive', 'derived_fork',
            'independent', 'ambiguous')),
    CONSTRAINT chk_plugin_flatten_rows_action
        CHECK (action IN ('archive_and_unlink', 'unlink_only', 'restore', 'skip')),
    -- `applied` means THIS run's compare-and-set claimed the row.
    -- `already_at_target` means the predicate matched nothing but the live row
    -- already held the intended end state — someone else's write, not this
    -- run's. Rollback restores only `applied`, so the two must not share a
    -- value: reverting a change the migration never made is the classic
    -- rollback that damages more than it undoes.
    CONSTRAINT chk_plugin_flatten_rows_state
        CHECK (row_state IN ('pending', 'applied', 'already_at_target', 'skipped', 'failed')),
    -- A skipped-by-plan row must carry its reason; a conflict must too. An
    -- unexplained skip is the one outcome an operator cannot act on.
    CONSTRAINT chk_plugin_flatten_rows_reason
        CHECK (action <> 'skip' OR reason <> '')
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_plugin_flatten_rows_seq
    ON plugin_flatten_migration_rows (run_id, seq);
CREATE INDEX IF NOT EXISTS idx_plugin_flatten_rows_pending
    ON plugin_flatten_migration_rows (run_id, seq)
    WHERE row_state = 'pending';
CREATE INDEX IF NOT EXISTS idx_plugin_flatten_rows_item
    ON plugin_flatten_migration_rows (item_id);

-- Converge a table created by an earlier draft of this file. `CREATE TABLE IF
-- NOT EXISTS` above is a no-op on an existing table, so a CHECK that gained a
-- value would never reach it.
ALTER TABLE plugin_flatten_migration_rows
    DROP CONSTRAINT IF EXISTS chk_plugin_flatten_rows_state;
ALTER TABLE plugin_flatten_migration_rows
    ADD CONSTRAINT chk_plugin_flatten_rows_state
    CHECK (row_state IN ('pending', 'applied', 'already_at_target', 'skipped', 'failed'));

COMMENT ON TABLE plugin_flatten_migration_rows IS 'Row-level plan and outcome for one plugin flatten run; before_* columns are the compare-and-set predicate';
COMMENT ON COLUMN plugin_flatten_migration_rows.before_parent_plugin_id IS 'Parent link inventoried at plan time; apply refuses the row if the live value has moved';
COMMENT ON COLUMN plugin_flatten_migration_rows.classification IS 'Provenance verdict; ambiguous is always paired with action=skip';
COMMENT ON COLUMN plugin_flatten_migration_rows.conflict IS 'Why a pending row became skipped at apply time, e.g. a concurrent change';
COMMENT ON COLUMN plugin_flatten_migration_rows.row_state IS 'pending -> applied (this run wrote it) | already_at_target (someone else had) | skipped | failed; only applied is rollback-eligible';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS plugin_flatten_migration_rows;
DROP TABLE IF EXISTS plugin_flatten_migration_runs;

-- +goose StatementEnd
