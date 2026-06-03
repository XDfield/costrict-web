package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

// Verbatim Facebook Ads example from the PRD: args carry an unlabeled bare path
// placeholder (index 0) and a flag-preceded token placeholder (index 2).
const fbAdsContent = `{"mcpServers":{"Facebook Ads":{"command":"python","args":["/path/to/your/fb-ads-mcp-server/server.py","--fb-token","YOUR_META_ACCESS_TOKEN"]}}}`

func newMCPRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	TagSvc = nil // skip tag enrichment (avoid stale global across tests)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	r.PUT("/api/items/:id/mcp-config", injectUser, UpsertMCPUserConfig)
	r.GET("/api/items/:id", injectUser, GetItem)
	return r
}

func seedMCPItem(id, slug, itemType, content string) {
	database.GetDB().Create(&models.CapabilityItem{
		ID:              id,
		RegistryID:      PublicRegistryID,
		RepoID:          "public",
		Slug:            slug,
		ItemType:        itemType,
		Name:            "Item " + slug,
		Descriptions:    datatypes.JSON([]byte(`{}`)),
		Content:         content,
		SourceType:      "direct",
		CreatedBy:       "owner",
		CurrentRevision: 1,
		Status:          "active",
		Version:         "1.0.0",
	})
}

func putMCPConfig(r *gin.Engine, itemID, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/items/"+itemID+"/mcp-config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func getItemReq(r *gin.Engine, itemID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/items/"+itemID, nil)
	r.ServeHTTP(w, req)
	return w
}

type mcpFieldRespEntry struct {
	Key      string `json:"key"`
	HasValue bool   `json:"hasValue"`
	Secret   bool   `json:"secret"`
	Value    string `json:"value"`
}

type mcpItemResp struct {
	ItemType  string `json:"itemType"`
	Content   string `json:"content"`
	MCPConfig *struct {
		Fields []mcpFieldRespEntry `json:"fields"`
	} `json:"mcpConfig"`
}

// serverArgs parses resolved .mcp.json content and returns the args of the
// (single) server it contains.
func serverArgs(t *testing.T, content string) []string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(content), &root); err != nil {
		t.Fatalf("content not json: %v\n%s", err, content)
	}
	servers, _ := root["mcpServers"].(map[string]any)
	for _, sv := range servers {
		server, _ := sv.(map[string]any)
		raw, _ := server["args"].([]any)
		out := make([]string, len(raw))
		for i, a := range raw {
			out[i], _ = a.(string)
		}
		return out
	}
	t.Fatalf("no server in content: %s", content)
	return nil
}

// ---------------------------------------------------------------------------
// resolveMCPContent (pure)
// ---------------------------------------------------------------------------

func TestResolveMCPContent(t *testing.T) {
	// args substitution at existing indices, flag (index 1) untouched.
	fields := map[string]mcpFieldEntry{
		"args:0": {V: "/Users/me/server.py", Secret: false},
		"args:2": {V: "tok-123", Secret: true},
	}
	got := serverArgs(t, resolveMCPContent(fbAdsContent, fields))
	want := []string{"/Users/me/server.py", "--fb-token", "tok-123"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("args[%d]: want %q, got %q", i, want[i], got[i])
		}
	}

	// env substitution only at existing keys.
	envContent := `{"mcpServers":{"x":{"command":"node","env":{"API_KEY":"YOUR_API_KEY"}}}}`
	resolved := resolveMCPContent(envContent, map[string]mcpFieldEntry{"env:API_KEY": {V: "secret-9", Secret: true}})
	if !strings.Contains(resolved, "secret-9") || strings.Contains(resolved, "YOUR_API_KEY") {
		t.Errorf("env not substituted: %s", resolved)
	}

	// non-existent env key must NOT be created.
	resolved2 := resolveMCPContent(envContent, map[string]mcpFieldEntry{"env:OTHER": {V: "x"}})
	if strings.Contains(resolved2, "OTHER") {
		t.Errorf("must not create absent env key: %s", resolved2)
	}

	// fail-safe: unparseable content returned verbatim.
	if out := resolveMCPContent("not json", fields); out != "not json" {
		t.Errorf("fail-safe: want original, got %q", out)
	}

	// out-of-range arg index ignored (no panic).
	got3 := serverArgs(t, resolveMCPContent(fbAdsContent, map[string]mcpFieldEntry{"args:9": {V: "x"}}))
	if got3[0] != "/path/to/your/fb-ads-mcp-server/server.py" {
		t.Errorf("out-of-range should leave args untouched, got %v", got3)
	}

	// empty fields → original.
	if out := resolveMCPContent(fbAdsContent, nil); out != fbAdsContent {
		t.Errorf("empty fields: want original")
	}
}

// ---------------------------------------------------------------------------
// buildMCPConfigStatus (masking)
// ---------------------------------------------------------------------------

func TestBuildMCPConfigStatus(t *testing.T) {
	if buildMCPConfigStatus(nil) != nil {
		t.Fatal("empty fields should yield nil status")
	}
	st := buildMCPConfigStatus(map[string]mcpFieldEntry{
		"env:TOKEN": {V: "real-token", Secret: true},
		"args:0":    {V: "/p/s.py", Secret: false},
	})
	if st == nil || len(st.Fields) != 2 {
		t.Fatalf("expected 2 fields, got %+v", st)
	}
	byKey := map[string]MCPFieldStatus{}
	for _, f := range st.Fields {
		byKey[f.Key] = f
	}
	if s := byKey["env:TOKEN"]; !s.Secret || !s.HasValue || s.Value != "real-token" {
		t.Errorf("secret field must expose its value (owner-only view): %+v", s)
	}
	if s := byKey["args:0"]; s.Secret || !s.HasValue || s.Value != "/p/s.py" {
		t.Errorf("non-secret field must expose value: %+v", s)
	}
}

// ---------------------------------------------------------------------------
// PUT contract + per-user resolution in GetItem
// ---------------------------------------------------------------------------

func TestUpsertMCPUserConfig_ContractAndResolve(t *testing.T) {
	defer setupTestDB(t)()
	seedMCPItem("fb-1", "facebook-ads", "mcp", fbAdsContent)
	seedMCPItem("sk-1", "a-skill", "skill", "plain content")

	validBody := `{"fields":{"args:0":{"v":"/Users/me/server.py","secret":false},"args:2":{"v":"tok-123","secret":true}}}`

	// Unauthenticated → 401.
	if w := putMCPConfig(newMCPRouter(""), "fb-1", validBody); w.Code != http.StatusUnauthorized {
		t.Errorf("unauth: want 401, got %d (%s)", w.Code, w.Body.String())
	}
	// Non-MCP item → 400.
	if w := putMCPConfig(newMCPRouter("alice"), "sk-1", validBody); w.Code != http.StatusBadRequest {
		t.Errorf("non-mcp: want 400, got %d (%s)", w.Code, w.Body.String())
	}
	// Missing item → 404.
	if w := putMCPConfig(newMCPRouter("alice"), "nope", validBody); w.Code != http.StatusNotFound {
		t.Errorf("missing: want 404, got %d (%s)", w.Code, w.Body.String())
	}

	// Valid save → 200, masked status (secret token has no value).
	w := putMCPConfig(newMCPRouter("alice"), "fb-1", validBody)
	if w.Code != http.StatusOK {
		t.Fatalf("save: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var saveResp struct {
		MCPConfig struct {
			Fields []mcpFieldRespEntry `json:"fields"`
		} `json:"mcpConfig"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &saveResp)
	got := map[string]mcpFieldRespEntry{}
	for _, f := range saveResp.MCPConfig.Fields {
		got[f.Key] = f
	}
	if f := got["args:2"]; !f.Secret || f.Value != "tok-123" {
		t.Errorf("token must be exposed in PUT resp (owner-only view): %+v", f)
	}
	if f := got["args:0"]; f.Secret || f.Value != "/Users/me/server.py" {
		t.Errorf("path must be exposed in PUT resp: %+v", f)
	}

	// GET as alice → content resolved with her values; mcpConfig present.
	var asAlice mcpItemResp
	_ = json.Unmarshal(getItemReq(newMCPRouter("alice"), "fb-1").Body.Bytes(), &asAlice)
	args := serverArgs(t, asAlice.Content)
	if args[0] != "/Users/me/server.py" || args[2] != "tok-123" {
		t.Errorf("alice content not resolved: %v", args)
	}
	if asAlice.MCPConfig == nil || len(asAlice.MCPConfig.Fields) != 2 {
		t.Errorf("alice should see mcpConfig status, got %+v", asAlice.MCPConfig)
	}

	// GET as bob (no config) → template content, no mcpConfig.
	var asBob mcpItemResp
	_ = json.Unmarshal(getItemReq(newMCPRouter("bob"), "fb-1").Body.Bytes(), &asBob)
	if !strings.Contains(asBob.Content, "YOUR_META_ACCESS_TOKEN") {
		t.Errorf("bob should get template, got %s", asBob.Content)
	}
	if asBob.MCPConfig != nil {
		t.Errorf("bob should have no mcpConfig, got %+v", asBob.MCPConfig)
	}

	// Shared row must NOT be mutated by per-user resolution.
	var stored models.CapabilityItem
	database.GetDB().Select("content").First(&stored, "id = ?", "fb-1")
	if stored.Content != fbAdsContent {
		t.Errorf("shared row content mutated! got %s", stored.Content)
	}

	// Clear one field (empty v) → merge keeps the other; template restored at that index.
	clearBody := `{"fields":{"args:0":{"v":"","secret":false}}}`
	if w := putMCPConfig(newMCPRouter("alice"), "fb-1", clearBody); w.Code != http.StatusOK {
		t.Fatalf("clear: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var afterClear mcpItemResp
	_ = json.Unmarshal(getItemReq(newMCPRouter("alice"), "fb-1").Body.Bytes(), &afterClear)
	args2 := serverArgs(t, afterClear.Content)
	if args2[0] != "/path/to/your/fb-ads-mcp-server/server.py" {
		t.Errorf("args:0 should be back to template after clear, got %q", args2[0])
	}
	if args2[2] != "tok-123" {
		t.Errorf("args:2 should persist after clearing args:0, got %q", args2[2])
	}

	// Invalid key ignored (still 200, not stored).
	if w := putMCPConfig(newMCPRouter("alice"), "fb-1", `{"fields":{"command:x":{"v":"evil"}}}`); w.Code != http.StatusOK {
		t.Fatalf("invalid-key: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var afterBad mcpItemResp
	_ = json.Unmarshal(getItemReq(newMCPRouter("alice"), "fb-1").Body.Bytes(), &afterBad)
	for _, f := range afterBad.MCPConfig.Fields {
		if f.Key == "command:x" {
			t.Errorf("invalid key must not be stored")
		}
	}
}
