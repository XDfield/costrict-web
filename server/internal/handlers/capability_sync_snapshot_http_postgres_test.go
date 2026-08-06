package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/syncsnapshot"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The full wire path, over real HTTP against a real PostgreSQL.
//
// The service tests prove the snapshot is built and frozen correctly; this
// proves a client that only ever sees HTTP responses can reach the same
// conclusion. Those are different claims, and the gap between them is exactly
// where a serialization choice (HTML escaping, key order, an omitted null)
// would silently break the digest a device depends on before deleting
// anything.

var snapshotHTTPFixture = []string{
	`CREATE TABLE capability_items (
		id UUID PRIMARY KEY, item_type TEXT NOT NULL DEFAULT 'skill', slug TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL DEFAULT '', version TEXT NOT NULL DEFAULT '',
		content_md5 VARCHAR(64) NOT NULL DEFAULT '', git_sha VARCHAR(40) NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'active', favorite_count INT NOT NULL DEFAULT 0)`,
	`CREATE TABLE item_favorites (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(), item_id UUID NOT NULL,
		user_id VARCHAR(191) NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE (item_id, user_id))`,
	`CREATE TABLE item_distributions (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(), item_id UUID NOT NULL,
		status VARCHAR(32) NOT NULL DEFAULT 'active')`,
	`CREATE TABLE item_distribution_receipts (
		id UUID PRIMARY KEY DEFAULT gen_random_uuid(), distribution_id UUID NOT NULL,
		user_id TEXT NOT NULL, receipt_status VARCHAR(32) NOT NULL DEFAULT 'unread')`,
}

var snapshotHTTPMigrations = []string{
	"20260805000200_create_capability_sync_tombstones.sql",
	"20260805000300_create_capability_sync_snapshot_generations.sql",
	"20260805000700_constrain_capability_sync_tombstone_triples.sql",
	"20260805000800_materialize_capability_sync_snapshots.sql",
}

func newSnapshotHTTPDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL capability sync snapshot HTTP test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	schema := fmt.Sprintf("sync_snapshot_http_%d", time.Now().UnixNano())

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
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
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL in %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	for _, ddl := range snapshotHTTPFixture {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create fixture: %v", err)
		}
	}
	for _, name := range snapshotHTTPMigrations {
		raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		body := string(raw)
		start := strings.Index(body, "-- +goose Up")
		if start < 0 {
			t.Fatalf("migration %s has no Up section", name)
		}
		body = body[start+len("-- +goose Up"):]
		if end := strings.Index(body, "-- +goose Down"); end >= 0 {
			body = body[:end]
		}
		if err := db.Exec(body).Error; err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
	return db
}

// httpSnapshotPage is the response as a verifying client parses it: raw
// elements, because the digest is over bytes.
type httpSnapshotPage struct {
	ContractVersion int               `json:"contractVersion"`
	SnapshotID      string            `json:"snapshotId"`
	Generation      int64             `json:"generation"`
	GeneratedAt     string            `json:"generatedAt"`
	PageIndex       int               `json:"pageIndex"`
	PageCount       int               `json:"pageCount"`
	ItemCount       int               `json:"itemCount"`
	TombstoneCount  int               `json:"tombstoneCount"`
	SnapshotDigest  string            `json:"snapshotDigest"`
	Complete        bool              `json:"complete"`
	Items           []json.RawMessage `json:"items"`
	Tombstones      []json.RawMessage `json:"tombstones"`
}

// TestCapabilitySyncSnapshot_HTTPRoundTripVerifies drives the endpoint the way
// csc will: fetch page 0, pin the snapshot id, collect the rest, reassemble,
// and verify counts and digest — entirely from HTTP response bodies.
func TestCapabilitySyncSnapshot_HTTPRoundTripVerifies(t *testing.T) {
	db := newSnapshotHTTPDB(t)
	const principal = "http-user"

	// One capability is named with characters that force the escaping question:
	// `<` and `&` (which gin's default encoder rewrites) and a supplementary
	// plane character.
	names := []string{"plain one", "<b>&amp;</b> two", "中文 \U0001D11E three", "four", "five"}
	for i, name := range names {
		id := fmt.Sprintf("%08d-0000-4000-8000-00000000000a", i+1)
		if err := db.Exec(`INSERT INTO capability_items (id, item_type, slug, name, version, content_md5)
			VALUES (?, 'skill', ?, ?, '1.0.0', ?)`, id, fmt.Sprintf("slug-%d", i+1), name,
			strings.Repeat(fmt.Sprint(i+1), 32)).Error; err != nil {
			t.Fatalf("seed item: %v", err)
		}
		if err := db.Exec(`INSERT INTO item_favorites (item_id, user_id) VALUES (?, ?)`, id, principal).Error; err != nil {
			t.Fatalf("seed favorite: %v", err)
		}
	}

	svc := &services.CapabilitySyncSnapshotService{DB: db, PageSize: 2, LifecyclePropagation: true}
	handler := NewCapabilitySyncSnapshotHandler(svc, true)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/sync/v2/snapshot", func(c *gin.Context) {
		c.Set(middleware.UserIDKey, principal)
		c.Next()
	}, handler.GetCapabilitySyncSnapshot)
	server := httptest.NewServer(router)
	defer server.Close()

	fetch := func(t *testing.T, query string) httpSnapshotPage {
		t.Helper()
		resp, err := http.Get(server.URL + "/api/sync/v2/snapshot" + query)
		if err != nil {
			t.Fatalf("GET %s: %v", query, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", query, resp.StatusCode)
		}
		var page httpSnapshotPage
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatalf("decode %s: %v", query, err)
		}
		return page
	}

	first := fetch(t, "")
	if first.PageCount != 3 {
		t.Fatalf("page count = %d, want 3 (5 items at page size 2)", first.PageCount)
	}
	if first.Complete {
		t.Fatal("page 0 of a 3-page snapshot must not be complete")
	}

	items := append([]json.RawMessage(nil), first.Items...)
	tombstones := append([]json.RawMessage(nil), first.Tombstones...)
	complete := first.Complete
	for index := 1; index < first.PageCount; index++ {
		page := fetch(t, fmt.Sprintf("?snapshotId=%s&page=%d", first.SnapshotID, index))
		if page.SnapshotID != first.SnapshotID || page.Generation != first.Generation ||
			page.SnapshotDigest != first.SnapshotDigest || page.PageCount != first.PageCount ||
			page.ItemCount != first.ItemCount || page.TombstoneCount != first.TombstoneCount {
			t.Fatalf("page %d did not repeat the manifest verbatim: %+v", index, page)
		}
		if page.PageIndex != index {
			t.Fatalf("page %d reports index %d", index, page.PageIndex)
		}
		items = append(items, page.Items...)
		tombstones = append(tombstones, page.Tombstones...)
		complete = page.Complete
	}
	if !complete {
		t.Fatal("the final page must carry complete=true")
	}
	if len(items) != first.ItemCount {
		t.Fatalf("reassembled %d items, manifest claims %d", len(items), first.ItemCount)
	}

	// The client's verification, from bytes that came off the wire.
	canonicalItems := make([]any, 0, len(items))
	for _, item := range items {
		canonical, err := syncsnapshot.CanonicalizeJSON(item)
		if err != nil {
			t.Fatalf("canonicalize transferred item: %v", err)
		}
		canonicalItems = append(canonicalItems, syncsnapshot.Raw(canonical))
	}
	canonicalTombstones := make([]any, 0, len(tombstones))
	for _, tombstone := range tombstones {
		canonical, err := syncsnapshot.CanonicalizeJSON(tombstone)
		if err != nil {
			t.Fatalf("canonicalize transferred tombstone: %v", err)
		}
		canonicalTombstones = append(canonicalTombstones, syncsnapshot.Raw(canonical))
	}
	_, digest, err := syncsnapshot.Digest(syncsnapshot.DocumentFor(syncsnapshot.Manifest{
		SnapshotID:     first.SnapshotID,
		Generation:     first.Generation,
		GeneratedAt:    first.GeneratedAt,
		PageCount:      first.PageCount,
		ItemCount:      first.ItemCount,
		TombstoneCount: first.TombstoneCount,
	}, canonicalItems, canonicalTombstones))
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if digest != first.SnapshotDigest {
		t.Fatalf("digest recomputed from HTTP bodies = %s, server advertised %s", digest, first.SnapshotDigest)
	}

	// `<` and `&` must have survived the transport unescaped, so the bytes a
	// client hashes are literally the bytes the server hashed.
	var sawRawAngleBracket bool
	for _, item := range items {
		if strings.Contains(string(item), "<b>&amp;</b>") {
			sawRawAngleBracket = true
		}
		for _, escape := range []string{`\u003c`, `\u003e`, `\u0026`} {
			if strings.Contains(string(item), escape) {
				t.Fatalf("the response HTML-escaped a capability name (%s): %s", escape, item)
			}
		}
	}
	if !sawRawAngleBracket {
		t.Fatal("the fixture item with angle brackets did not come back intact")
	}

	// A page outside the snapshot is an error, not an empty page: an empty page
	// on a complete snapshot is what "you are entitled to nothing" looks like.
	resp, err := http.Get(fmt.Sprintf("%s/api/sync/v2/snapshot?snapshotId=%s&page=%d",
		server.URL, first.SnapshotID, first.PageCount))
	if err != nil {
		t.Fatalf("GET out-of-range page: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("out-of-range page status = %d, want 400", resp.StatusCode)
	}

	// An expired snapshot answers 410 (restart from page 0), never a rebuilt
	// page under the old id.
	if err := db.Exec(`UPDATE capability_sync_snapshots SET expires_at = now() - interval '1 minute' WHERE id = ?`,
		first.SnapshotID).Error; err != nil {
		t.Fatalf("expire snapshot: %v", err)
	}
	resp, err = http.Get(fmt.Sprintf("%s/api/sync/v2/snapshot?snapshotId=%s&page=1", server.URL, first.SnapshotID))
	if err != nil {
		t.Fatalf("GET expired snapshot: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expired snapshot status = %d, want 410", resp.StatusCode)
	}
}
