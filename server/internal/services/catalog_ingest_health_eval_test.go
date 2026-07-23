package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// newIngestTestDB builds an in-memory SQLite DB with exactly the tables the
// CatalogIngestService touches. The schema mirrors the hand-written DDL used
// by scan_service_test.go (so the new health/evaluation columns are present)
// plus the registry / version / tag / category / scan_job tables the ingest
// write paths exercise.
func newIngestTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE capability_registries (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, description TEXT,
			source_type TEXT NOT NULL DEFAULT 'internal', external_url TEXT,
			external_branch TEXT DEFAULT 'main', sync_enabled INTEGER DEFAULT 0,
			sync_interval INTEGER DEFAULT 3600, last_synced_at DATETIME,
			last_sync_sha TEXT, sync_status TEXT DEFAULT 'idle',
			sync_config TEXT DEFAULT '{}', last_sync_log_id TEXT,
			repo_id TEXT, owner_id TEXT NOT NULL,
			created_at DATETIME, updated_at DATETIME
		)`,
		`CREATE TABLE capability_items (
			id TEXT PRIMARY KEY, registry_id TEXT NOT NULL, repo_id TEXT NOT NULL,
			slug TEXT NOT NULL, item_type TEXT NOT NULL, name TEXT NOT NULL,
			description TEXT, descriptions TEXT NOT NULL DEFAULT '{}', category TEXT,
			version TEXT DEFAULT '1.0.0', content TEXT, content_md5 TEXT DEFAULT '',
			current_revision INTEGER NOT NULL DEFAULT 1, metadata TEXT DEFAULT '{}',
			health TEXT DEFAULT '{}', evaluation TEXT DEFAULT '{}',
			source_path TEXT, catalog_entry_dir TEXT NOT NULL DEFAULT '', source_sha TEXT, source_type TEXT DEFAULT 'direct',
			source TEXT DEFAULT '', forked_from_item_id TEXT, forked_from_owner_id TEXT, parent_plugin_id TEXT, is_built_in INTEGER DEFAULT 0, preview_count INTEGER DEFAULT 0,
			install_count INTEGER DEFAULT 0, favorite_count INTEGER DEFAULT 0,
			status TEXT DEFAULT 'active', security_status TEXT DEFAULT 'unscanned',
			last_scan_id TEXT, created_by TEXT NOT NULL, updated_by TEXT,
			embedding TEXT, experience_score REAL DEFAULT 0,
			embedding_updated_at DATETIME, created_at DATETIME, updated_at DATETIME,
			UNIQUE(repo_id, item_type, slug)
		)`,
		`CREATE TABLE capability_versions (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, revision INTEGER NOT NULL,
			name TEXT, description TEXT, descriptions TEXT NOT NULL DEFAULT '{}',
			category TEXT, version TEXT, content TEXT NOT NULL,
			content_md5 TEXT DEFAULT '', metadata TEXT DEFAULT '{}',
			commit_msg TEXT, created_by TEXT NOT NULL, source_path TEXT,
			created_at DATETIME
		)`,
		`CREATE TABLE capability_assets (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, rel_path TEXT NOT NULL,
			text_content TEXT, storage_backend TEXT DEFAULT '',
			storage_key TEXT, mime_type TEXT, file_size INTEGER DEFAULT 0,
			content_sha TEXT, created_at DATETIME, updated_at DATETIME
		)`,
		`CREATE TABLE item_tag_dicts (
			id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE,
			tag_class TEXT NOT NULL DEFAULT 'custom', created_by TEXT NOT NULL,
			created_at DATETIME
		)`,
		`CREATE TABLE item_tags (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, tag_id TEXT NOT NULL,
			created_at DATETIME, UNIQUE(item_id, tag_id)
		)`,
		`CREATE TABLE item_categories (
			id TEXT PRIMARY KEY, slug TEXT NOT NULL UNIQUE, icon TEXT,
			sort_order INTEGER DEFAULT 0, names TEXT NOT NULL DEFAULT '{}',
			descriptions TEXT DEFAULT '{}', created_by TEXT NOT NULL,
			created_at DATETIME, updated_at DATETIME
		)`,
		`CREATE TABLE scan_jobs (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, item_revision INTEGER NOT NULL DEFAULT 0,
			trigger_type TEXT NOT NULL, trigger_user TEXT, priority INTEGER NOT NULL DEFAULT 5,
			status TEXT NOT NULL DEFAULT 'pending', retry_count INTEGER DEFAULT 0,
			max_attempts INTEGER DEFAULT 2, last_error TEXT, scheduled_at DATETIME NOT NULL,
			started_at DATETIME, finished_at DATETIME, scan_result_id TEXT, created_at DATETIME
		)`,
		`CREATE TABLE security_scans (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, item_revision INTEGER NOT NULL DEFAULT 0,
			trigger_type TEXT NOT NULL, scan_model TEXT, category TEXT DEFAULT '',
			builtin_tags TEXT DEFAULT '[]', risk_level TEXT DEFAULT '',
			verdict TEXT DEFAULT '', red_flags TEXT DEFAULT '[]',
			permissions TEXT DEFAULT '{}', summary TEXT,
			recommendations TEXT DEFAULT '[]', raw_output TEXT,
			duration_ms INTEGER DEFAULT 0, created_at DATETIME, finished_at DATETIME
		)`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func newIngestService(db *gorm.DB) *CatalogIngestService {
	return &CatalogIngestService{
		DB:             db,
		Parser:         &ParserService{},
		TagSvc:         &TagService{DB: db},
		CategorySvc:    &CategoryService{DB: db},
		ScanJobService: &ScanJobService{DB: db},
	}
}

// writeSkillBundle materializes a one-entry catalog bundle (a single skill)
// into a temp dir laid out exactly like the upstream bundle: manifest.json,
// index.json, and catalog-download/skills/<id>/SKILL.md. The index entry is
// `entry` (with whatever health/evaluation blocks the caller set), and the
// SKILL.md body is `skillBody` so callers can vary the file SHA when they
// want a content change vs. a metadata-only pass.
func writeSkillBundle(t *testing.T, entry catalogEntry, skillBody string) string {
	t.Helper()
	dir := t.TempDir()

	manifest := map[string]any{
		"schema_version": SupportedBundleSchemaVersion,
		"generated_at":   "2026-05-25T00:00:00Z",
		"entry_count":    1,
		"index_sha256":   "test-sha",
		"type_counts":    map[string]int{entry.Type: 1},
	}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), mb, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	ib, _ := json.Marshal([]catalogEntry{entry})
	if err := os.WriteFile(filepath.Join(dir, "index.json"), ib, 0o644); err != nil {
		t.Fatalf("write index: %v", err)
	}

	skillDir := filepath.Join(dir, "catalog-download", "skills", entry.ID)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(skillBody), 0o644); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return dir
}

// sampleHealth / sampleEvaluation return the raw upstream JSON shape. Each
// deliberately includes an EXTRA field NOT present in the old fixed structs
// (signals.install_popularity, evaluation_mode) so the tests prove the
// raw-passthrough preserves the whole upstream object end-to-end.
func sampleHealth() json.RawMessage {
	return json.RawMessage(`{"score":0.82,"signals":{"freshness":0.9,"popularity":0.7,"source_trust":0.85,"install_popularity":42},"freshness_label":"fresh","last_commit":"2026-05-01T00:00:00Z"}`)
}

func sampleEvaluation() json.RawMessage {
	return json.RawMessage(`{"coding_relevance":4.5,"doc_completeness":4.0,"final_score":4.2,"decision":"accept","model_id":"deepseek-v4","rubric_version":"v2","evaluated_at":"2026-05-10T00:00:00Z","evaluation_mode":"deep"}`)
}

func loadItemBySlug(t *testing.T, db *gorm.DB, slug string) models.CapabilityItem {
	t.Helper()
	var item models.CapabilityItem
	if err := db.Where("slug = ?", slug).First(&item).Error; err != nil {
		t.Fatalf("load item %q: %v", slug, err)
	}
	return item
}

// decodeObj unmarshals a stored jsonb column into a generic map so tests can
// assert on arbitrary upstream fields without a fixed struct.
func decodeObj(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal jsonb: %v (raw=%s)", err, string(raw))
	}
	return m
}

// 4.1 — an ingest entry carrying health + evaluation results in the
// capability_items row having the correctly serialized JSON after create,
// and after a subsequent content change (updateItem path).
func TestIngest_HealthEvaluation_PersistedOnCreate(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	skillBody := "---\nname: Demo Skill\ndescription: does X\n---\n# Demo\nbody v1\n"
	entry := catalogEntry{
		ID:          "demo-skill",
		Type:        "skill",
		Source:      "anthropic/demo",
		Description: "does X",
		Category:    "tooling",
		Tags:        []string{"demo"},
		FinalScore:  4.2,
		Health:      sampleHealth(),
		Evaluation:  sampleEvaluation(),
	}
	dir := writeSkillBundle(t, entry, skillBody)

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Added != 1 {
		t.Fatalf("expected Added=1, got Added=%d updated=%d failed=%d errors=%v", res.Added, res.Updated, res.Failed, res.Errors)
	}

	item := loadItemBySlug(t, db, "demo-skill")

	gotHealth := decodeObj(t, item.Health)
	if gotHealth["score"] != 0.82 || gotHealth["freshness_label"] != "fresh" {
		t.Fatalf("health not serialized correctly: %+v (raw=%s)", gotHealth, string(item.Health))
	}
	signals, ok := gotHealth["signals"].(map[string]any)
	if !ok || signals["popularity"] != 0.7 {
		t.Fatalf("health.signals not serialized correctly: %+v (raw=%s)", gotHealth, string(item.Health))
	}
	// Extra upstream field must survive the ingest verbatim.
	if signals["install_popularity"] != float64(42) {
		t.Fatalf("extra health field install_popularity dropped: %+v (raw=%s)", signals, string(item.Health))
	}

	gotEval := decodeObj(t, item.Evaluation)
	if gotEval["decision"] != "accept" || gotEval["final_score"] != 4.2 || gotEval["coding_relevance"] != 4.5 {
		t.Fatalf("evaluation not serialized correctly: %+v (raw=%s)", gotEval, string(item.Evaluation))
	}
	if gotEval["evaluation_mode"] != "deep" {
		t.Fatalf("extra evaluation field evaluation_mode dropped: %+v (raw=%s)", gotEval, string(item.Evaluation))
	}

	// Now re-ingest with a changed file body (different SHA) AND mutated
	// health/evaluation — exercises the updateItem content-changed path.
	entry.Health = json.RawMessage(`{"score":0.5,"freshness_label":"stale","signals":{"freshness":0.4,"popularity":0.3,"source_trust":0.5}}`)
	entry.Evaluation = json.RawMessage(`{"final_score":2.0,"decision":"reject","model_id":"deepseek-v4"}`)
	dir2 := writeSkillBundle(t, entry, skillBody+"\nbody v2\n")

	res2, err := svc.Ingest(context.Background(), IngestSource{Dir: dir2}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("re-ingest (content change): %v", err)
	}
	if res2.Updated != 1 {
		t.Fatalf("expected Updated=1 on content change, got added=%d updated=%d metadataUpdated=%d failed=%d errors=%v",
			res2.Added, res2.Updated, res2.MetadataUpdated, res2.Failed, res2.Errors)
	}

	item2 := loadItemBySlug(t, db, "demo-skill")
	gotHealth2 := decodeObj(t, item2.Health)
	if gotHealth2["score"] != 0.5 || gotHealth2["freshness_label"] != "stale" {
		t.Fatalf("updateItem did not refresh health: %+v (raw=%s)", gotHealth2, string(item2.Health))
	}
	gotEval2 := decodeObj(t, item2.Evaluation)
	if gotEval2["decision"] != "reject" {
		t.Fatalf("updateItem did not refresh evaluation: %+v (raw=%s)", gotEval2, string(item2.Evaluation))
	}
}

// 4.2 — an entry WITHOUT health/evaluation does not error and the columns are
// `{}` (empty object), never null.
func TestIngest_NoHealthEvaluation_ColumnsAreEmptyObject(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	skillBody := "---\nname: Plain Skill\ndescription: no extras\n---\n# Plain\nbody\n"
	entry := catalogEntry{
		ID:          "plain-skill",
		Type:        "skill",
		Source:      "anthropic/plain",
		Description: "no extras",
		Category:    "tooling",
		FinalScore:  3.0,
		// Health and Evaluation intentionally nil.
	}
	dir := writeSkillBundle(t, entry, skillBody)

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Added != 1 || res.Failed != 0 {
		t.Fatalf("expected clean add, got added=%d failed=%d errors=%v", res.Added, res.Failed, res.Errors)
	}

	item := loadItemBySlug(t, db, "plain-skill")
	if string(item.Health) != "{}" {
		t.Fatalf("expected health '{}', got %q", string(item.Health))
	}
	if string(item.Evaluation) != "{}" {
		t.Fatalf("expected evaluation '{}', got %q", string(item.Evaluation))
	}
}

// 4.3 — regression for the P1 fix. An existing row with empty {} health/eval
// and an UNCHANGED file SHA + metadata must NOT be skipped when a later bundle
// carries health/evaluation: the metadata-only path detects the jsonb drift
// and backfills the columns.
func TestIngest_MetadataOnlyPath_BackfillsHealthEvaluation(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	// First pass: no health/evaluation (columns end up '{}').
	skillBody := "---\nname: Backfill Skill\ndescription: stable\n---\n# Backfill\nstable body\n"
	entry := catalogEntry{
		ID:          "backfill-skill",
		Type:        "skill",
		Source:      "anthropic/backfill",
		Description: "stable",
		Category:    "tooling",
		FinalScore:  3.5,
	}
	dir := writeSkillBundle(t, entry, skillBody)
	if _, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"}); err != nil {
		t.Fatalf("first ingest: %v", err)
	}
	before := loadItemBySlug(t, db, "backfill-skill")
	if string(before.Health) != "{}" || string(before.Evaluation) != "{}" {
		t.Fatalf("setup expected empty health/eval, got health=%q eval=%q", string(before.Health), string(before.Evaluation))
	}
	beforeRevision := before.CurrentRevision

	// Second pass: identical file body (same SHA) and identical metadata,
	// but now carrying health + evaluation. This MUST route through the
	// metadata-only path (file SHA unchanged) and update the columns rather
	// than skip the item.
	entry.Health = sampleHealth()
	entry.Evaluation = sampleEvaluation()
	dir2 := writeSkillBundle(t, entry, skillBody)

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir2}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	if res.MetadataUpdated != 1 {
		t.Fatalf("expected MetadataUpdated=1 (not skipped), got added=%d updated=%d metadataUpdated=%d skipped=%d errors=%v",
			res.Added, res.Updated, res.MetadataUpdated, res.Skipped, res.Errors)
	}
	if res.Skipped != 0 {
		t.Fatalf("item must NOT be skipped — health/eval drift should trigger metadata update; skipped=%d", res.Skipped)
	}
	if res.Updated != 0 {
		t.Fatalf("file SHA unchanged so the content path must not fire; updated=%d", res.Updated)
	}

	after := loadItemBySlug(t, db, "backfill-skill")
	gotHealth := decodeObj(t, after.Health)
	if gotHealth["score"] != 0.82 || gotHealth["freshness_label"] != "fresh" {
		t.Fatalf("metadata-only path did not backfill health: %+v (raw=%s)", gotHealth, string(after.Health))
	}
	// Extra upstream field preserved through the metadata-only backfill path too.
	if signals, ok := gotHealth["signals"].(map[string]any); !ok || signals["install_popularity"] != float64(42) {
		t.Fatalf("metadata-only backfill dropped extra health field: %+v (raw=%s)", gotHealth, string(after.Health))
	}
	gotEval := decodeObj(t, after.Evaluation)
	if gotEval["decision"] != "accept" {
		t.Fatalf("metadata-only path did not backfill evaluation: %+v (raw=%s)", gotEval, string(after.Evaluation))
	}
	if gotEval["evaluation_mode"] != "deep" {
		t.Fatalf("metadata-only backfill dropped extra evaluation field: %+v (raw=%s)", gotEval, string(after.Evaluation))
	}
	// Metadata-only path must not bump the revision (no new version row).
	if after.CurrentRevision != beforeRevision {
		t.Fatalf("metadata-only path must not bump revision: before=%d after=%d", beforeRevision, after.CurrentRevision)
	}
}

// rawBlockJSON must enforce the "JSON object" column contract: empty forms,
// invalid JSON, AND valid-but-wrong scalar/array payloads all normalize to
// "{}". A non-empty object passes through (whitespace-compacted) unchanged.
func TestRawBlockJSON_ObjectContract(t *testing.T) {
	cases := []struct {
		name string
		in   json.RawMessage
		want string
	}{
		{"nil", nil, "{}"},
		{"empty bytes", json.RawMessage(``), "{}"},
		{"json null", json.RawMessage(`null`), "{}"},
		{"empty object", json.RawMessage(`{}`), "{}"},
		{"array", json.RawMessage(`[1,2]`), "{}"},
		{"string scalar", json.RawMessage(`"x"`), "{}"},
		{"number scalar", json.RawMessage(`42`), "{}"},
		{"bool scalar", json.RawMessage(`true`), "{}"},
		{"invalid json", json.RawMessage(`{not json`), "{}"},
		{"object passthrough", json.RawMessage(`{ "a" : 1 }`), `{"a":1}`},
		{"object with extra field", json.RawMessage(`{"score":0.5,"extra":[1,2]}`), `{"score":0.5,"extra":[1,2]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(rawBlockJSON(tc.in)); got != tc.want {
				t.Fatalf("rawBlockJSON(%s) = %q, want %q", string(tc.in), got, tc.want)
			}
		})
	}
}

// 4.x — non-object health/evaluation payloads must end up stored as "{}",
// proven end-to-end through the real ingest (not just the unit helper).
func TestIngest_NonObjectHealthEvaluation_StoredAsEmptyObject(t *testing.T) {
	db := newIngestTestDB(t)
	svc := newIngestService(db)

	skillBody := "---\nname: Wrong Shape\ndescription: bad upstream\n---\n# Wrong\nbody\n"
	entry := catalogEntry{
		ID:          "wrong-shape",
		Type:        "skill",
		Source:      "anthropic/wrong",
		Description: "bad upstream",
		Category:    "tooling",
		FinalScore:  2.0,
		Health:      json.RawMessage(`[1,2]`),
		Evaluation:  json.RawMessage(`"x"`),
	}
	dir := writeSkillBundle(t, entry, skillBody)

	res, err := svc.Ingest(context.Background(), IngestSource{Dir: dir}, IngestOptions{TriggerUser: "tester"})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Added != 1 || res.Failed != 0 {
		t.Fatalf("expected clean add, got added=%d failed=%d errors=%v", res.Added, res.Failed, res.Errors)
	}

	item := loadItemBySlug(t, db, "wrong-shape")
	if string(item.Health) != "{}" {
		t.Fatalf("array health must be rejected to '{}', got %q", string(item.Health))
	}
	if string(item.Evaluation) != "{}" {
		t.Fatalf("scalar evaluation must be rejected to '{}', got %q", string(item.Evaluation))
	}
}
