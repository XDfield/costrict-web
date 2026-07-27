//go:build s3e2e

package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/storage"
)

type s3E2ERequest struct {
	Method           string
	Path             string
	RawQuery         string
	ContentLength    int64
	Header           http.Header
	Trailer          http.Header
	TransferEncoding []string
}

type s3E2ERecorder struct {
	mu       sync.Mutex
	requests []s3E2ERequest
	proxy    *httputil.ReverseProxy
}

func newS3E2ERecorder(t *testing.T, endpoint string) *s3E2ERecorder {
	t.Helper()
	target, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse S3_ENDPOINT: %v", err)
	}
	if target.Scheme == "" || target.Host == "" {
		t.Fatalf("S3_ENDPOINT must be an absolute URL, got %q", endpoint)
	}
	return &s3E2ERecorder{proxy: httputil.NewSingleHostReverseProxy(target)}
}

func (r *s3E2ERecorder) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	observed := s3E2ERequest{
		Method:           request.Method,
		Path:             request.URL.Path,
		RawQuery:         request.URL.RawQuery,
		ContentLength:    request.ContentLength,
		Header:           request.Header.Clone(),
		TransferEncoding: append([]string(nil), request.TransferEncoding...),
	}

	if reason := forbiddenS3E2ERequest(observed); reason != "" {
		r.record(observed)
		http.Error(w, reason, http.StatusMethodNotAllowed)
		return
	}

	r.proxy.ServeHTTP(w, request)
	observed.Trailer = request.Trailer.Clone()
	r.record(observed)
}

func (r *s3E2ERecorder) record(request s3E2ERequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, request)
}

func (r *s3E2ERecorder) snapshot() []s3E2ERequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]s3E2ERequest(nil), r.requests...)
}

func forbiddenS3E2ERequest(request s3E2ERequest) string {
	var expectedOperation string
	switch request.Method {
	case http.MethodPut:
		expectedOperation = "PutObject"
	case http.MethodGet:
		expectedOperation = "GetObject"
	default:
		return fmt.Sprintf("S3 operation %s is forbidden", request.Method)
	}

	query, err := url.ParseQuery(request.RawQuery)
	if err != nil {
		return fmt.Sprintf("S3 query is invalid: %s", request.RawQuery)
	}
	operationValues, exists := query["x-id"]
	if len(query) != 1 || !exists || len(operationValues) != 1 || operationValues[0] != expectedOperation {
		return fmt.Sprintf(
			"S3 query must be exactly x-id=%s for %s: %s",
			expectedOperation,
			request.Method,
			request.RawQuery,
		)
	}
	if len(request.TransferEncoding) != 0 {
		return fmt.Sprintf("S3 transfer encoding is forbidden: %v", request.TransferEncoding)
	}
	for name, values := range request.Header {
		lowerName := strings.ToLower(name)
		lowerValues := strings.ToLower(strings.Join(values, ","))
		if strings.Contains(lowerName, "crc32") ||
			strings.Contains(lowerName, "checksum") ||
			lowerName == "x-amz-trailer" ||
			strings.Contains(lowerValues, "crc32") ||
			strings.Contains(lowerValues, "checksum") ||
			strings.Contains(lowerValues, "aws-chunked") {
			return fmt.Sprintf("S3 checksum or chunked header is forbidden: %s=%q", name, values)
		}
	}
	return ""
}

func requireS3E2EEnv(t *testing.T, key string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		t.Fatalf("%s is required for the s3e2e test", key)
	}
	return value
}

func writeS3E2ECatalogBundle(t *testing.T, mainContent, scriptContent string, png []byte) string {
	t.Helper()
	bundleDir := t.TempDir()
	entryDir := filepath.Join(bundleDir, "catalog-download", "skills", "s3-catalog-skill")
	if err := os.MkdirAll(filepath.Join(entryDir, "scripts"), 0o755); err != nil {
		t.Fatalf("create script directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(entryDir, "assets"), 0o755); err != nil {
		t.Fatalf("create asset directory: %v", err)
	}

	manifest := map[string]any{
		"schema_version": services.SupportedBundleSchemaVersion,
		"generated_at":   "2026-07-23T00:00:00Z",
		"entry_count":    1,
		"index_sha256":   strings.Repeat("a", 64),
		"type_counts":    map[string]int{"skill": 1},
	}
	index := []map[string]any{{
		"id":          "s3-catalog-skill",
		"type":        "skill",
		"source":      "s3-e2e",
		"description": "S3 distribution E2E",
	}}
	writeJSON := func(name string, value any) {
		t.Helper()
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(bundleDir, name), content, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	writeJSON("manifest.json", manifest)
	writeJSON("index.json", index)

	files := map[string][]byte{
		"SKILL.md":         []byte(mainContent),
		"scripts/setup.sh": []byte(scriptContent),
		"assets/icon.png":  png,
	}
	for relPath, content := range files {
		if err := os.WriteFile(filepath.Join(entryDir, filepath.FromSlash(relPath)), content, 0o644); err != nil {
			t.Fatalf("write catalog file %s: %v", relPath, err)
		}
	}
	return bundleDir
}

func sha256String(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func TestS3CatalogSkillDistribution(t *testing.T) {
	endpoint := requireS3E2EEnv(t, "S3_ENDPOINT")
	bucket := requireS3E2EEnv(t, "S3_BUCKET")
	requireS3E2EEnv(t, "S3_REGION")
	requireS3E2EEnv(t, "S3_FORCE_PATH_STYLE")
	requireS3E2EEnv(t, "AWS_ACCESS_KEY_ID")
	requireS3E2EEnv(t, "AWS_SECRET_ACCESS_KEY")

	recorder := newS3E2ERecorder(t, endpoint)
	proxyServer := httptest.NewServer(recorder)
	defer proxyServer.Close()

	t.Setenv("ARTIFACT_STORAGE_BACKEND", storage.KindS3)
	t.Setenv("S3_ENDPOINT", proxyServer.URL)
	backend, err := storage.NewFromEnv(context.Background())
	if err != nil {
		t.Fatalf("create production S3 backend: %v", err)
	}

	defer setupTestDB(t)()
	oldBackend := StorageBackend
	StorageBackend = backend
	defer func() { StorageBackend = oldBackend }()

	mainContent := "---\nname: S3 Catalog Skill\ndescription: S3 E2E\n---\n# S3 Catalog Skill\n"
	scriptContent := "#!/bin/sh\necho from-db\n"
	png := []byte{
		0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
		0x00, 0x00, 0x00, 0x0d, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	}
	bundleDir := writeS3E2ECatalogBundle(t, mainContent, scriptContent, png)

	ingest := &services.CatalogIngestService{
		DB:      database.DB,
		Parser:  &services.ParserService{},
		Storage: backend,
	}
	result, err := ingest.Ingest(
		context.Background(),
		services.IngestSource{Dir: bundleDir},
		services.IngestOptions{TriggerUser: "s3-e2e"},
	)
	if err != nil {
		t.Fatalf("ingest catalog bundle: %v", err)
	}
	if result.Added != 1 || result.Failed != 0 {
		t.Fatalf("unexpected ingest result: %+v", result)
	}

	var item models.CapabilityItem
	if err := database.DB.Where("slug = ?", "s3-catalog-skill").First(&item).Error; err != nil {
		t.Fatalf("load ingested item: %v", err)
	}
	if item.Content != mainContent {
		t.Fatalf("main text was not stored in DB: got %q", item.Content)
	}

	var assets []models.CapabilityAsset
	if err := database.DB.Where("item_id = ?", item.ID).Find(&assets).Error; err != nil {
		t.Fatalf("load DB asset mappings: %v", err)
	}
	if len(assets) != 2 {
		t.Fatalf("DB asset count = %d, want 2: %+v", len(assets), assets)
	}
	assetsByPath := make(map[string]models.CapabilityAsset, len(assets))
	for _, asset := range assets {
		assetsByPath[asset.RelPath] = asset
	}

	script := assetsByPath["scripts/setup.sh"]
	if script.TextContent == nil || *script.TextContent != scriptContent {
		t.Fatalf("text asset was not stored in DB: %+v", script)
	}
	if script.StorageBackend != "" || script.StorageKey != "" {
		t.Fatalf("text asset unexpectedly references object storage: %+v", script)
	}
	if script.FileSize != int64(len(scriptContent)) || script.ContentSHA != sha256String([]byte(scriptContent)) {
		t.Fatalf("text asset DB size/SHA mismatch: %+v", script)
	}

	binary := assetsByPath["assets/icon.png"]
	if binary.TextContent != nil {
		t.Fatalf("binary asset unexpectedly stored in DB text: %+v", binary)
	}
	if binary.StorageBackend != storage.KindS3 || binary.StorageKey == "" {
		t.Fatalf("binary asset S3 mapping is incomplete: %+v", binary)
	}
	if binary.FileSize != int64(len(png)) || binary.ContentSHA != sha256String(png) {
		t.Fatalf("binary asset DB size/SHA mismatch: %+v", binary)
	}
	expectedStorageKey := fmt.Sprintf(
		"catalog/%s/assets/%s/assets/icon.png",
		item.ID,
		sha256String(png),
	)
	if binary.StorageKey != expectedStorageKey {
		t.Fatalf("binary asset storage key = %q, want deterministic DB mapping %q", binary.StorageKey, expectedStorageKey)
	}

	afterIngest := recorder.snapshot()
	if len(afterIngest) != 1 || afterIngest[0].Method != http.MethodPut {
		t.Fatalf("ingest must perform exactly one PUT, got %+v", afterIngest)
	}

	// Subsequent reads must be independent of the materialized catalog source.
	if err := os.WriteFile(
		filepath.Join(bundleDir, "catalog-download", "skills", "s3-catalog-skill", "scripts", "setup.sh"),
		[]byte("tampered source"),
		0o644,
	); err != nil {
		t.Fatalf("tamper source script: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(bundleDir, "catalog-download", "skills", "s3-catalog-skill", "assets", "icon.png"),
		[]byte("tampered source"),
		0o644,
	); err != nil {
		t.Fatalf("tamper source PNG: %v", err)
	}

	router := newRouter("")
	router.GET("/api/items/:id/assets", ListItemAssets)

	manifestResponse := get(router, "/api/items/"+item.ID+"/assets")
	if manifestResponse.Code != http.StatusOK {
		t.Fatalf("asset manifest status = %d: %s", manifestResponse.Code, manifestResponse.Body.String())
	}
	var manifest struct {
		Assets []map[string]any `json:"assets"`
	}
	if err := json.Unmarshal(manifestResponse.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("decode asset manifest: %v", err)
	}
	if len(manifest.Assets) != 2 {
		t.Fatalf("asset manifest count = %d, want 2: %#v", len(manifest.Assets), manifest.Assets)
	}
	manifestByPath := make(map[string]map[string]any, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		relPath, _ := asset["relPath"].(string)
		manifestByPath[relPath] = asset
		if _, exists := asset["storageKey"]; exists {
			t.Fatalf("asset manifest leaks storageKey: %#v", asset)
		}
		if _, exists := asset["storageBackend"]; exists {
			t.Fatalf("asset manifest leaks storageBackend: %#v", asset)
		}
	}
	for relPath, dbAsset := range assetsByPath {
		asset, exists := manifestByPath[relPath]
		if !exists {
			t.Fatalf("asset manifest is missing %q: %#v", relPath, manifest.Assets)
		}
		if asset["fileSize"] != float64(dbAsset.FileSize) || asset["contentSha"] != dbAsset.ContentSHA {
			t.Fatalf("manifest size/SHA for %q does not match DB: %#v vs %+v", relPath, asset, dbAsset)
		}
	}

	mainResponse := get(router, "/api/registry/public/skill/s3-catalog-skill/SKILL.md")
	if mainResponse.Code != http.StatusOK || mainResponse.Body.String() != mainContent {
		t.Fatalf("main DB download failed: status=%d body=%q", mainResponse.Code, mainResponse.Body.String())
	}
	scriptResponse := get(router, "/api/registry/public/skill/s3-catalog-skill/scripts/setup.sh")
	if scriptResponse.Code != http.StatusOK || scriptResponse.Body.String() != scriptContent {
		t.Fatalf("text DB download failed: status=%d body=%q", scriptResponse.Code, scriptResponse.Body.String())
	}
	if requests := recorder.snapshot(); len(requests) != 1 {
		t.Fatalf("manifest and text downloads must not access S3, got %+v", requests)
	}

	pngResponse := get(router, "/api/registry/public/skill/s3-catalog-skill/assets/icon.png")
	if pngResponse.Code != http.StatusOK {
		t.Fatalf("PNG download status = %d: %s", pngResponse.Code, pngResponse.Body.String())
	}
	if got := pngResponse.Body.Bytes(); string(got) != string(png) {
		t.Fatalf("PNG download bytes differ: got %x want %x", got, png)
	}
	if got := pngResponse.Header().Get("Content-Length"); got != strconv.Itoa(len(png)) {
		t.Fatalf("PNG Content-Length = %q, want %d", got, len(png))
	}
	if got := sha256String(pngResponse.Body.Bytes()); got != manifestByPath["assets/icon.png"]["contentSha"] {
		t.Fatalf("PNG SHA-256 = %q, manifest has %q", got, manifestByPath["assets/icon.png"]["contentSha"])
	}

	requests := recorder.snapshot()
	if len(requests) != 2 {
		t.Fatalf("expected exactly one PUT and one GET, got %+v", requests)
	}
	expectedPath := "/" + bucket + "/" + binary.StorageKey
	if requests[0].Method != http.MethodPut || requests[1].Method != http.MethodGet {
		t.Fatalf("S3 methods = %s, %s; want PUT, GET", requests[0].Method, requests[1].Method)
	}
	for _, request := range requests {
		if request.Path != expectedPath {
			t.Errorf("%s S3 path = %q, want %q", request.Method, request.Path, expectedPath)
		}
		if reason := forbiddenS3E2ERequest(request); reason != "" {
			t.Errorf("%s request violated the S3 contract: %s", request.Method, reason)
		}
		if len(request.Trailer) != 0 {
			t.Errorf("%s sent forbidden S3 trailers: %v", request.Method, request.Trailer)
		}
	}
	if requests[0].ContentLength != int64(len(png)) {
		t.Errorf("PUT Content-Length = %d, want %d", requests[0].ContentLength, len(png))
	}
}
