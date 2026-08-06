// The same claim as capability_item_git_lifecycle_wire_test.go, but against a
// real PostgreSQL instead of in-memory SQLite.
//
// It is not a duplicate. The two lifecycle columns are `varchar(32) NULL` and
// `timestamptz NULL`, and everything interesting about them happens at the
// driver boundary: a NULL text column landing in a *string, a timestamptz
// landing in a *time.Time and being re-rendered as RFC 3339 with a zone. SQLite
// stores both as loosely typed values and would not notice a scan bug that
// PostgreSQL surfaces immediately.
//
// Runs in a throwaway schema created and dropped by the test, so the shared
// local database is never written to. Skips when DATABASE_URL is unset.

package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func newGitLifecyclePostgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL git lifecycle projection test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	schema := fmt.Sprintf("git_lifecycle_wire_%d", time.Now().UnixNano())

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	quoted := `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`
	if err := admin.Exec("CREATE SCHEMA " + quoted).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + quoted + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
		// The schema under test is one table's columns, not the relational
		// graph: emitting FK constraints only makes AutoMigrate order-sensitive
		// across a model set this test deliberately keeps small.
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("open PostgreSQL in %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	// AutoMigrate rather than hand-written DDL: the point of this test is that
	// the columns the production schema actually has round-trip, so the schema
	// must come from the same model definitions production migrates from.
	if err := db.AutoMigrate(
		&models.Repository{}, &models.RepoMember{}, &models.CapabilityRegistry{},
		&models.CapabilityItem{}, &models.CapabilityVersion{}, &models.CapabilityAsset{},
		&models.CapabilityArtifact{}, &models.GitServer{}, &models.ItemFavorite{},
	); err != nil {
		t.Fatalf("automigrate: %v", err)
	}

	previousDB, previousTagSvc := database.DB, TagSvc
	database.DB, TagSvc = db, nil
	resetGitVisibilityCache()
	t.Cleanup(func() {
		database.DB, TagSvc = previousDB, previousTagSvc
		resetGitVisibilityCache()
	})
	return db
}

// assertPostgresColumn fails unless the live schema really has the type the
// projection is being tested against. Without it a silent model change could
// turn this into a test of some other column shape.
func assertPostgresColumn(t *testing.T, db *gorm.DB, column, wantType string, wantNullable bool) {
	t.Helper()
	var got struct {
		DataType   string
		IsNullable string
	}
	if err := db.Raw(`SELECT data_type, is_nullable FROM information_schema.columns
		WHERE table_name = 'capability_items' AND column_name = ?
		  AND table_schema = current_schema()`, column).Scan(&got).Error; err != nil {
		t.Fatalf("introspect %s: %v", column, err)
	}
	if got.DataType != wantType {
		t.Fatalf("capability_items.%s is %q, expected %q", column, got.DataType, wantType)
	}
	if nullable := got.IsNullable == "YES"; nullable != wantNullable {
		t.Fatalf("capability_items.%s nullable=%v, expected %v", column, nullable, wantNullable)
	}
}

// PostgreSQL types these primary keys as uuid, so the fixture uses real UUIDs
// rather than the readable strings the SQLite suite gets away with.
const (
	pgLifecycleItemID     = "aaaaaaaa-0000-4000-8000-00000000ffff"
	pgLifecycleRepoID     = "bbbbbbbb-0000-4000-8000-00000000ffff"
	pgLifecycleRegistryID = "cccccccc-0000-4000-8000-00000000ffff"
)

func seedPostgresGitItem(t *testing.T, db *gorm.DB, itemID, giteaURL string) {
	t.Helper()
	if err := db.Create(&models.Repository{
		ID: pgLifecycleRepoID, Name: "repo-" + itemID, Visibility: "public", OwnerID: "u1",
	}).Error; err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	if err := db.Create(&models.CapabilityRegistry{
		ID: pgLifecycleRegistryID, Name: "reg-" + itemID, SourceType: "git",
		RepoID: pgLifecycleRepoID, OwnerID: "u1",
	}).Error; err != nil {
		t.Fatalf("seed registry: %v", err)
	}
	if err := db.Exec(
		`INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
		 VALUES (?, 'gitea', ?, 'fake', '{"admin_token":"admin-token"}', false, true, now(), now())`,
		gitContentTestServerID, giteaURL).Error; err != nil {
		t.Fatalf("seed git_servers: %v", err)
	}
	if err := db.Create(&models.CapabilityItem{
		ID: itemID, RegistryID: pgLifecycleRegistryID, RepoID: pgLifecycleRepoID, Slug: "pg-lifecycle",
		ItemType: "skill", Name: "Git " + itemID, Status: "active", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")), Descriptions: datatypes.JSON([]byte("{}")),
		Health: datatypes.JSON([]byte("{}")), Evaluation: datatypes.JSON([]byte("{}")),
		Content: gitContentStaleDBValue, CurrentRevision: 1,
		ContentBackend: models.ContentBackendGit,
		SourceRepoURL:  "https://gitea.example.test/" + gitContentTestRepoName,
		SourceRepoRef:  "main", SourceRepoPath: "skill.md",
		SourceGitServerID: gitContentTestServerID, SourceGitRepoID: gitContentTestRepoID,
		GitSHA: strings.Repeat("a", 40), GitSyncStatus: "synced",
	}).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
}

// TestGetItem_GitLifecycleClaimRoundTripsThroughPostgres drives the real
// handler over real HTTP against real PostgreSQL rows.
func TestGetItem_GitLifecycleClaimRoundTripsThroughPostgres(t *testing.T) {
	db := newGitLifecyclePostgresDB(t)

	// The column shapes the contract depends on.
	assertPostgresColumn(t, db, "git_lifecycle_reason", "character varying", true)
	assertPostgresColumn(t, db, "git_lifecycle_changed_at", "timestamp with time zone", true)

	gitea := newFakeContentGitea("admin-token")
	srv := httptest.NewServer(gitea)
	defer srv.Close()
	gitea.setFile("skill.md", gitContentSkillFile)

	seedPostgresGitItem(t, db, pgLifecycleItemID, srv.URL)

	// NULL first: the healthy row must emit neither key, which is what the hub
	// reads as "Git has made no claim about this capability".
	body := getPostgresItem(t, pgLifecycleItemID)
	if _, present := body["gitLifecycleReason"]; present {
		t.Fatalf("NULL reason surfaced as a key: %v", body["gitLifecycleReason"])
	}
	if _, present := body["gitLifecycleChangedAt"]; present {
		t.Fatalf("NULL changed_at surfaced as a key: %v", body["gitLifecycleChangedAt"])
	}

	// Now the claim, written the way the sync service writes it — including the
	// microsecond precision PostgreSQL stores and SQLite would round away.
	changedAt := time.Date(2026, 8, 6, 9, 30, 15, 123456000, time.UTC)
	for _, reason := range []string{
		models.GitLifecycleReasonManifestRemoved,
		models.GitLifecycleReasonDefaultBranchMissing,
		models.GitLifecycleReasonRepositoryDeleted,
	} {
		if err := db.Exec(`UPDATE capability_items
			SET git_lifecycle_reason = ?, git_lifecycle_changed_at = ? WHERE id = ?`,
			reason, changedAt, pgLifecycleItemID).Error; err != nil {
			t.Fatalf("write claim %s: %v", reason, err)
		}

		body := getPostgresItem(t, pgLifecycleItemID)
		if body["gitLifecycleReason"] != reason {
			t.Fatalf("reason %s did not reach the wire: %v", reason, body["gitLifecycleReason"])
		}
		raw, ok := body["gitLifecycleChangedAt"].(string)
		if !ok {
			t.Fatalf("reason %s: changed_at missing: %v", reason, body["gitLifecycleChangedAt"])
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			t.Fatalf("reason %s: changed_at is not RFC 3339: %q (%v)", reason, raw, err)
		}
		if !parsed.UTC().Equal(changedAt) {
			t.Fatalf("reason %s: changed_at lost precision through PostgreSQL: got %s want %s",
				reason, parsed.UTC().Format(time.RFC3339Nano), changedAt.Format(time.RFC3339Nano))
		}
		// The claim rides alongside the sync state, not instead of it.
		if body["gitSyncStatus"] != "synced" {
			t.Fatalf("reason %s: gitSyncStatus lost: %v", reason, body["gitSyncStatus"])
		}
		if body["contentBackend"] != "git" {
			t.Fatalf("reason %s: contentBackend lost: %v", reason, body["contentBackend"])
		}
	}
}

func getPostgresItem(t *testing.T, itemID string) map[string]any {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/items/:id", func(c *gin.Context) {
		c.Set(middleware.UserIDKey, "u1")
		c.Next()
	}, GetItem)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/items/"+itemID, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for %s, got %d: %s", itemID, w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return body
}

// TestTrustedGitOrigins_ProjectsRealPostgresJSONBConfig covers the other half
// of the wire gap on the engine that actually runs it.
//
// git_servers.config is `jsonb` in PostgreSQL and plain TEXT in the SQLite
// suite, and the Go field is a `string` either way — so the SQLite tests never
// prove the driver hands the blob back as parseable text. They also cannot
// prove the one thing PostgreSQL settles for free: jsonb REFUSES to store
// invalid JSON, which makes the "unreadable config" branch unreachable in
// production rather than merely unlikely. That is asserted here, not assumed.
func TestTrustedGitOrigins_ProjectsRealPostgresJSONBConfig(t *testing.T) {
	db := newGitLifecyclePostgresDB(t)

	insert := func(serverID, endpoint, config string, enabled bool) error {
		return db.Exec(
			`INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
			 VALUES (?, 'gitea', ?, ?, ?, false, ?, now(), now())`,
			serverID, endpoint, serverID, config, enabled).Error
	}

	if err := insert("gs-split", "http://gitea.costrict.svc.cluster.local:3000",
		`{"admin_token":"SECRET","web_url":"https://gitea.costrict.ai"}`, true); err != nil {
		t.Fatalf("seed split-address server: %v", err)
	}
	if err := insert("gs-single", "http://localhost:3001", `{"admin_token":"SECRET"}`, true); err != nil {
		t.Fatalf("seed single-address server: %v", err)
	}
	if err := insert("gs-drained", "https://drained.example.test", `{"admin_token":"SECRET"}`, false); err != nil {
		t.Fatalf("seed drained server: %v", err)
	}

	// jsonb is a parser, not a text column: a malformed blob is rejected at the
	// write, so no row can reach the read path unreadable.
	if err := insert("gs-broken", "https://broken.example.test", `{not json`, true); err == nil {
		t.Fatal("PostgreSQL accepted an invalid jsonb config; the skip branch is reachable after all")
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/git-servers/trusted-origins", ListTrustedGitOrigins)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/git-servers/trusted-origins", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		Origins []string `json:"origins"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	want := []string{"https://gitea.costrict.ai", "http://localhost:3001"}
	if len(body.Origins) != len(want) {
		t.Fatalf("expected %v, got %v", want, body.Origins)
	}
	got := map[string]bool{}
	for _, origin := range body.Origins {
		got[origin] = true
	}
	for _, origin := range want {
		if !got[origin] {
			t.Fatalf("missing %q from %v", origin, body.Origins)
		}
	}

	raw := w.Body.String()
	for _, forbidden := range []string{"SECRET", "gitea.costrict.svc.cluster.local", "drained.example.test", "gs-split"} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, raw)
		}
	}
}
