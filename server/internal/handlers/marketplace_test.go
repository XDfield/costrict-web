package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

// newMarketplaceRouter creates a router for marketplace testing
func newMarketplaceRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}

	// Setup services
	db := database.GetDB()
	searchSvc := services.NewSearchService(db, &config.SearchConfig{})
	behaviorSvc := services.NewBehaviorService(db)
	recommendSvc := services.NewRecommendService(db, behaviorSvc, searchSvc)

	// Search routes
	searchHandler := NewSearchHandler(searchSvc)
	r.POST("/api/marketplace/items/search", injectUser, searchHandler.SemanticSearch)
	r.POST("/api/marketplace/items/hybrid-search", injectUser, searchHandler.HybridSearch)
	r.GET("/api/items/:id/similar", injectUser, searchHandler.FindSimilar)

	// Recommend routes
	recommendHandler := NewRecommendHandler(recommendSvc, behaviorSvc)
	r.POST("/api/marketplace/items/recommend", injectUser, recommendHandler.GetRecommendations)
	r.GET("/api/marketplace/items/trending", injectUser, recommendHandler.GetTrending)
	r.GET("/api/marketplace/items/new", injectUser, recommendHandler.GetNewAndNoteworthy)
	r.POST("/api/items/:id/behavior", injectUser, recommendHandler.LogBehavior)
	r.POST("/api/items/:id/favorite", injectUser, recommendHandler.FavoriteItem)
	r.DELETE("/api/items/:id/favorite", injectUser, recommendHandler.UnfavoriteItem)
	r.GET("/api/items/:id/stats", injectUser, recommendHandler.GetItemStats)

	return r
}

// ---------------------------------------------------------------------------
// SemanticSearch
// ---------------------------------------------------------------------------

func TestSemanticSearch_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-search", Name: "search-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-search1",
		RegistryID:  "reg-search",
		Slug:        "test-skill",
		ItemType:    "skill",
		Name:        "Test Skill",
		Description: "A test skill for searching",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("")
	w := postJSON(r, "/api/marketplace/items/search", map[string]interface{}{
		"query": "test",
		"limit": 10,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSemanticSearch_MissingQuery(t *testing.T) {
	defer setupTestDB(t)()
	r := newMarketplaceRouter("")
	w := postJSON(r, "/api/marketplace/items/search", map[string]interface{}{
		"limit": 10,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestSemanticSearch_WithFilters(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-filter", Name: "filter-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-filter1",
		RegistryID:  "reg-filter",
		Slug:        "filter-skill",
		ItemType:    "skill",
		Name:        "Filter Skill",
		Description: "A skill for filtering",
		Category:    "testing",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("")
	w := postJSON(r, "/api/marketplace/items/search", map[string]interface{}{
		"query":      "filter",
		"limit":      10,
		"types":      []string{"skill"},
		"categories": []string{"testing"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// HybridSearch
// ---------------------------------------------------------------------------

func TestHybridSearch_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-hybrid", Name: "hybrid-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-hybrid",
		RegistryID:  "reg-hybrid",
		Slug:        "hybrid-skill",
		ItemType:    "skill",
		Name:        "Hybrid Search Skill",
		Description: "A skill for hybrid search",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("")
	w := postJSON(r, "/api/marketplace/items/hybrid-search", map[string]interface{}{
		"query": "hybrid",
		"limit": 10,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHybridSearch_MissingQuery(t *testing.T) {
	defer setupTestDB(t)()
	r := newMarketplaceRouter("")
	w := postJSON(r, "/api/marketplace/items/hybrid-search", map[string]interface{}{
		"limit": 10,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GetRecommendations
// ---------------------------------------------------------------------------

func TestGetRecommendations_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-rec", Name: "rec-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-rec1",
		RegistryID:  "reg-rec",
		Slug:        "rec-skill",
		ItemType:    "skill",
		Name:        "Recommended Skill",
		Description: "A recommended skill",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("")
	w := postJSON(r, "/api/marketplace/items/recommend", map[string]interface{}{
		"limit": 10,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetRecommendations_WithTypes(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-rec2", Name: "rec-reg2", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:         "item-rec-skill",
		RegistryID: "reg-rec2",
		Slug:       "skill-type",
		ItemType:   "skill",
		Name:       "Skill Type",
		Status:     "active",
		CreatedBy:  "u1",
		Metadata:   datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID:         "item-rec-cmd",
		RegistryID: "reg-rec2",
		Slug:       "cmd-type",
		ItemType:   "command",
		Name:       "Command Type",
		Status:     "active",
		CreatedBy:  "u1",
		Metadata:   datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("")
	w := postJSON(r, "/api/marketplace/items/recommend", map[string]interface{}{
		"limit": 10,
		"types": []string{"skill"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// GetTrending
// ---------------------------------------------------------------------------

func TestGetTrending_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-trend", Name: "trend-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-trend1",
		RegistryID:  "reg-trend",
		Slug:        "trending-skill",
		ItemType:    "skill",
		Name:        "Trending Skill",
		Description: "A trending skill",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("")
	w := get(r, "/api/marketplace/items/trending")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetTrending_WithLimit(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-trend2", Name: "trend-reg2", SourceType: "internal", OwnerID: "u1",
	})
	for i := 0; i < 5; i++ {
		database.DB.Create(&models.CapabilityItem{
			ID:         "item-trend" + string(rune('a'+i)),
			RegistryID: "reg-trend2",
			Slug:       "trending-skill-" + string(rune('a'+i)),
			ItemType:   "skill",
			Name:       "Trending Skill",
			Status:     "active",
			CreatedBy:  "u1",
			Metadata:   datatypes.JSON([]byte("{}")),
		})
	}

	r := newMarketplaceRouter("")
	w := get(r, "/api/marketplace/items/trending?limit=3")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// GetNewAndNoteworthy
// ---------------------------------------------------------------------------

func TestGetNewAndNoteworthy_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-new", Name: "new-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-new1",
		RegistryID:  "reg-new",
		Slug:        "new-skill",
		ItemType:    "skill",
		Name:        "New Skill",
		Description: "A new skill",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("")
	w := get(r, "/api/marketplace/items/new")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetNewAndNoteworthy_WithLimit(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-new2", Name: "new-reg2", SourceType: "internal", OwnerID: "u1",
	})
	for i := 0; i < 5; i++ {
		database.DB.Create(&models.CapabilityItem{
			ID:         "item-new" + string(rune('b'+i)),
			RegistryID: "reg-new2",
			Slug:       "new-skill-" + string(rune('b'+i)),
			ItemType:   "skill",
			Name:       "New Skill",
			Status:     "active",
			CreatedBy:  "u1",
			Metadata:   datatypes.JSON([]byte("{}")),
		})
	}

	r := newMarketplaceRouter("")
	w := get(r, "/api/marketplace/items/new?limit=2")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// FindSimilar
// ---------------------------------------------------------------------------

func TestFindSimilar_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-similar", Name: "similar-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-similar1",
		RegistryID:  "reg-similar",
		Slug:        "similar-skill",
		ItemType:    "skill",
		Name:        "Similar Skill",
		Description: "A skill for similarity",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-similar2",
		RegistryID:  "reg-similar",
		Slug:        "another-skill",
		ItemType:    "skill",
		Name:        "Another Skill",
		Description: "Another skill for similarity",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("")
	w := get(r, "/api/items/item-similar1/similar")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestFindSimilar_ItemNotFound(t *testing.T) {
	defer setupTestDB(t)()
	r := newMarketplaceRouter("")
	w := get(r, "/api/items/nonexistent-item/similar")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// LogBehavior
// ---------------------------------------------------------------------------

func TestLogBehavior_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-behavior", Name: "behavior-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:         "item-behavior",
		RegistryID: "reg-behavior",
		Slug:       "behavior-skill",
		ItemType:   "skill",
		Name:       "Behavior Skill",
		Status:     "active",
		CreatedBy:  "u1",
		Metadata:   datatypes.JSON([]byte("{}")),
	})

	// Authenticated view increments the denormalized preview_count.
	r := newMarketplaceRouter("user-v")
	w := postJSON(r, "/api/items/item-behavior/behavior", map[string]interface{}{
		"actionType": "view",
		"durationMs": 5000,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		var item models.CapabilityItem
		if err := database.DB.First(&item, "id = ?", "item-behavior").Error; err != nil {
			t.Fatalf("failed to load item: %v", err)
		}
		if item.PreviewCount == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected preview_count=1 eventually, got %d", item.PreviewCount)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// SRC-2026-4791: anonymous view/click are logged for telemetry but must NOT move
// the denormalized preview_count, which is public and sortable — otherwise an
// unauthenticated caller could forge it to game item-list ranking.
func TestLogBehavior_AnonymousViewDoesNotCount(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-anonview", Name: "anonview-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:         "item-anonview",
		RegistryID: "reg-anonview",
		Slug:       "anonview-skill",
		ItemType:   "skill",
		Name:       "AnonView Skill",
		Status:     "active",
		CreatedBy:  "u1",
		Metadata:   datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("") // anonymous
	w := postJSON(r, "/api/items/item-anonview/behavior", map[string]interface{}{
		"actionType": "view",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Give any async counter update a chance to (wrongly) fire, then assert it did not.
	time.Sleep(100 * time.Millisecond)
	var item models.CapabilityItem
	if err := database.DB.First(&item, "id = ?", "item-anonview").Error; err != nil {
		t.Fatalf("failed to load item: %v", err)
	}
	if item.PreviewCount != 0 {
		t.Fatalf("expected preview_count=0 for anonymous view, got %d", item.PreviewCount)
	}
}

func TestLogBehavior_MissingActionType(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-beh2", Name: "beh-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:         "item-beh2",
		RegistryID: "reg-beh2",
		Slug:       "beh-skill",
		ItemType:   "skill",
		Name:       "Beh Skill",
		Status:     "active",
		CreatedBy:  "u1",
		Metadata:   datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("")
	w := postJSON(r, "/api/items/item-beh2/behavior", map[string]interface{}{
		"durationMs": 5000,
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestLogBehavior_ItemNotFound(t *testing.T) {
	defer setupTestDB(t)()
	r := newMarketplaceRouter("")
	w := postJSON(r, "/api/items/nonexistent-item/behavior", map[string]interface{}{
		"actionType": "view",
	})
	// Item doesn't exist but behavior can still be logged (item_id will be NULL in DB)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

// SRC-2026-4791: trust/counting action types must reject anonymous writes so
// install counts, ratings and success rates can't be forged without a login.
func TestLogBehavior_AnonymousTrustActionRejected(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-anon", Name: "anon-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:         "item-anon",
		RegistryID: "reg-anon",
		Slug:       "anon-skill",
		ItemType:   "skill",
		Name:       "Anon Skill",
		Status:     "active",
		CreatedBy:  "u1",
		Metadata:   datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("") // anonymous

	for _, action := range []string{"install", "success", "fail", "use", "feedback", "ignore"} {
		w := postJSON(r, "/api/items/item-anon/behavior", map[string]interface{}{
			"actionType": action,
			"rating":     5,
			"feedback":   "amazing tool",
		})
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("action %q: expected 401, got %d: %s", action, w.Code, w.Body.String())
		}
	}

	// No trust rows should have been written, and counters must stay at zero.
	var logCount int64
	database.DB.Model(&models.BehaviorLog{}).Where("item_id = ?", "item-anon").Count(&logCount)
	if logCount != 0 {
		t.Fatalf("expected 0 behavior logs for item, got %d", logCount)
	}
	var item models.CapabilityItem
	database.DB.First(&item, "id = ?", "item-anon")
	if item.InstallCount != 0 {
		t.Fatalf("expected install_count=0, got %d", item.InstallCount)
	}
}

// view/click remain allowed anonymously so marketplace browsing telemetry keeps
// working; they are excluded from stats aggregation elsewhere.
func TestLogBehavior_AnonymousBrowseActionAllowed(t *testing.T) {
	defer setupTestDB(t)()
	r := newMarketplaceRouter("") // anonymous
	for _, action := range []string{"view", "click"} {
		w := postJSON(r, "/api/items/some-item/behavior", map[string]interface{}{
			"actionType": action,
		})
		if w.Code != http.StatusCreated {
			t.Fatalf("action %q: expected 201, got %d: %s", action, w.Code, w.Body.String())
		}
	}
}

func TestLogBehavior_AuthenticatedTrustActionSucceeds(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-authbeh", Name: "authbeh-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:         "item-authbeh",
		RegistryID: "reg-authbeh",
		Slug:       "authbeh-skill",
		ItemType:   "skill",
		Name:       "AuthBeh Skill",
		Status:     "active",
		CreatedBy:  "u1",
		Metadata:   datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("user-authbeh") // authenticated
	w := postJSON(r, "/api/items/item-authbeh/behavior", map[string]interface{}{
		"actionType": "install",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestLogBehavior_InvalidActionType(t *testing.T) {
	defer setupTestDB(t)()
	r := newMarketplaceRouter("user-x")
	w := postJSON(r, "/api/items/some-item/behavior", map[string]interface{}{
		"actionType": "delete-everything",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// Oversized feedback must be truncated on a rune boundary so we never persist
// invalid UTF-8 (which PostgreSQL rejects on insert). 400 Chinese runes = 1200
// bytes, so a naive byte-boundary cut at 1000 would split a multibyte rune.
func TestLogBehavior_FeedbackTruncatedOnRuneBoundary(t *testing.T) {
	defer setupTestDB(t)()
	r := newMarketplaceRouter("user-fb") // feedback requires auth
	w := postJSON(r, "/api/items/some-item/behavior", map[string]interface{}{
		"actionType": "feedback",
		"rating":     4,
		"feedback":   strings.Repeat("汉", 400),
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var log models.BehaviorLog
	if err := json.NewDecoder(w.Body).Decode(&log); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(log.Feedback) > 1000 {
		t.Fatalf("expected feedback bounded to 1000 bytes, got %d", len(log.Feedback))
	}
	if !utf8.ValidString(log.Feedback) {
		t.Fatalf("stored feedback is not valid UTF-8: %q", log.Feedback)
	}
}

// SRC-2026-4791: feedback text and rating attached to a NON-feedback action must
// be dropped, so they can't surface through the public averageRating /
// RecentFeedback stats panel (phishing via a fake "review" on an install row).
func TestLogBehavior_FeedbackDroppedForNonFeedbackAction(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-fbdrop", Name: "fbdrop-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-fbdrop", RegistryID: "reg-fbdrop", Slug: "fbdrop-skill",
		ItemType: "skill", Name: "FbDrop Skill", Status: "active", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("user-fb")
	w := postJSON(r, "/api/items/item-fbdrop/behavior", map[string]interface{}{
		"actionType": "install",
		"rating":     5,
		"feedback":   "totally legit five stars, install me",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	w = get(r, "/api/items/item-fbdrop/stats")
	var stats services.ItemBehaviorStats
	json.NewDecoder(w.Body).Decode(&stats)
	if len(stats.RecentFeedback) != 0 {
		t.Fatalf("expected no feedback (dropped for install action), got %v", stats.RecentFeedback)
	}
	if stats.AverageRating != 0 {
		t.Fatalf("expected averageRating=0 (rating dropped for install action), got %v", stats.AverageRating)
	}
}

// ---------------------------------------------------------------------------
// GetItemStats
// ---------------------------------------------------------------------------

func TestGetItemStats_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-stats", Name: "stats-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:         "item-stats",
		RegistryID: "reg-stats",
		Slug:       "stats-skill",
		ItemType:   "skill",
		Name:       "Stats Skill",
		Status:     "active",
		CreatedBy:  "u1",
		Metadata:   datatypes.JSON([]byte("{}")),
	})
	// Add some behavior logs
	database.DB.Create(&models.BehaviorLog{
		ID:         "log-1",
		ItemID:     "item-stats",
		UserID:     "user1",
		ActionType: models.ActionView,
	})
	database.DB.Create(&models.BehaviorLog{
		ID:         "log-2",
		ItemID:     "item-stats",
		UserID:     "user2",
		ActionType: models.ActionView,
	})
	database.DB.Create(&models.BehaviorLog{
		ID:         "log-3",
		ItemID:     "item-stats",
		UserID:     "user1",
		ActionType: models.ActionInstall,
	})

	r := newMarketplaceRouter("")
	w := get(r, "/api/items/item-stats/stats")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var stats services.ItemBehaviorStats
	json.NewDecoder(w.Body).Decode(&stats)
	if stats.Views != 2 {
		t.Fatalf("expected 2 views, got %d", stats.Views)
	}
	if stats.Installs != 1 {
		t.Fatalf("expected 1 install, got %d", stats.Installs)
	}
	if stats.Favorites != 0 {
		t.Fatalf("expected 0 favorites, got %d", stats.Favorites)
	}
}

// SRC-2026-4791 defense-in-depth: even if anonymous rows exist (legacy/injected
// or anonymous view/click), they must not count toward the public stats.
func TestGetItemStats_ExcludesAnonymous(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-statsanon", Name: "statsanon-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:         "item-statsanon",
		RegistryID: "reg-statsanon",
		Slug:       "statsanon-skill",
		ItemType:   "skill",
		Name:       "StatsAnon Skill",
		Status:     "active",
		CreatedBy:  "u1",
		Metadata:   datatypes.JSON([]byte("{}")),
	})
	// One legit authenticated install + a pile of forged anonymous rows.
	database.DB.Create(&models.BehaviorLog{
		ID: "sa-real", ItemID: "item-statsanon", UserID: "user1", ActionType: models.ActionInstall,
	})
	database.DB.Create(&models.BehaviorLog{
		ID: "sa-anon-install", ItemID: "item-statsanon", UserID: models.AnonymousUserID, ActionType: models.ActionInstall,
	})
	database.DB.Create(&models.BehaviorLog{
		ID: "sa-anon-success", ItemID: "item-statsanon", UserID: models.AnonymousUserID, ActionType: models.ActionSuccess,
	})
	database.DB.Create(&models.BehaviorLog{
		ID: "sa-anon-feedback", ItemID: "item-statsanon", UserID: models.AnonymousUserID, ActionType: models.ActionFeedback, Rating: 5, Feedback: "amazing tool",
	})

	r := newMarketplaceRouter("")
	w := get(r, "/api/items/item-statsanon/stats")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var stats services.ItemBehaviorStats
	json.NewDecoder(w.Body).Decode(&stats)
	if stats.Installs != 1 {
		t.Fatalf("expected 1 install (anonymous excluded), got %d", stats.Installs)
	}
	if stats.Successes != 0 {
		t.Fatalf("expected 0 successes (anonymous excluded), got %d", stats.Successes)
	}
	if stats.AverageRating != 0 {
		t.Fatalf("expected averageRating=0 (anonymous excluded), got %v", stats.AverageRating)
	}
	if len(stats.RecentFeedback) != 0 {
		t.Fatalf("expected no feedback (anonymous excluded), got %v", stats.RecentFeedback)
	}
}

func TestGetItemStats_NoData(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-stats2", Name: "stats-reg2", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:         "item-stats2",
		RegistryID: "reg-stats2",
		Slug:       "stats-skill2",
		ItemType:   "skill",
		Name:       "Stats Skill 2",
		Status:     "active",
		CreatedBy:  "u1",
		Metadata:   datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("")
	w := get(r, "/api/items/item-stats2/stats")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var stats services.ItemBehaviorStats
	json.NewDecoder(w.Body).Decode(&stats)
	if stats.Views != 0 {
		t.Fatalf("expected 0 views, got %d", stats.Views)
	}
}

func TestFavoriteItem_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{
		ID: "repo-fav", Name: "repo-fav", OwnerID: "u1", Visibility: "public",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-fav", Name: "fav-reg", SourceType: "internal", RepoID: "repo-fav", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-fav", RegistryID: "reg-fav", RepoID: "repo-fav", Slug: "fav-skill", ItemType: "skill",
		Name: "Favorite Skill", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("user-fav")
	w := postJSON(r, "/api/items/item-fav/favorite", map[string]interface{}{})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var item models.CapabilityItem
	if err := database.DB.First(&item, "id = ?", "item-fav").Error; err != nil {
		t.Fatalf("failed to load item: %v", err)
	}
	if item.FavoriteCount != 1 {
		t.Fatalf("expected favorite_count=1, got %d", item.FavoriteCount)
	}

	var count int64
	database.DB.Model(&models.ItemFavorite{}).Where("item_id = ? AND user_id = ?", "item-fav", "user-fav").Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 item_favorite row, got %d", count)
	}
}

func TestFavoriteItem_Idempotent(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{
		ID: "repo-fav2", Name: "repo-fav2", OwnerID: "u1", Visibility: "public",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-fav2", Name: "fav-reg2", SourceType: "internal", RepoID: "repo-fav2", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-fav2", RegistryID: "reg-fav2", RepoID: "repo-fav2", Slug: "fav-skill2", ItemType: "skill",
		Name: "Favorite Skill 2", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("user-fav2")
	postJSON(r, "/api/items/item-fav2/favorite", map[string]interface{}{})
	w := postJSON(r, "/api/items/item-fav2/favorite", map[string]interface{}{})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var item models.CapabilityItem
	if err := database.DB.First(&item, "id = ?", "item-fav2").Error; err != nil {
		t.Fatalf("failed to load item: %v", err)
	}
	if item.FavoriteCount != 1 {
		t.Fatalf("expected favorite_count=1 after duplicate favorite, got %d", item.FavoriteCount)
	}
}

func TestUnfavoriteItem_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{
		ID: "repo-unfav", Name: "repo-unfav", OwnerID: "u1", Visibility: "public",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-unfav", Name: "unfav-reg", SourceType: "internal", RepoID: "repo-unfav", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-unfav", RegistryID: "reg-unfav", RepoID: "repo-unfav", Slug: "unfav-skill", ItemType: "skill",
		Name: "Favorite Skill", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
		FavoriteCount: 1,
	})
	database.DB.Create(&models.ItemFavorite{
		ID: "fav-1", ItemID: "item-unfav", UserID: "user-unfav",
	})

	r := newMarketplaceRouter("user-unfav")
	w := deleteReq(r, "/api/items/item-unfav/favorite")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var item models.CapabilityItem
	if err := database.DB.First(&item, "id = ?", "item-unfav").Error; err != nil {
		t.Fatalf("failed to load item: %v", err)
	}
	if item.FavoriteCount != 0 {
		t.Fatalf("expected favorite_count=0, got %d", item.FavoriteCount)
	}
}

func TestFavoriteItem_Plugin(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{
		ID: "repo-plugin", Name: "repo-plugin", OwnerID: "u1", Visibility: "public",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-plugin", Name: "plugin-reg", SourceType: "internal", RepoID: "repo-plugin", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-plugin", RegistryID: "reg-plugin", RepoID: "repo-plugin", Slug: "demo-plugin", ItemType: "plugin",
		Name: "Demo Plugin", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte(`{"install":{"plugin_name":"demo"}}`)),
	})

	// Plugins are favoritable just like skills/mcp (csc reconciles them into the
	// native /plugin panel); the type-specific gate has been removed.
	r := newMarketplaceRouter("user-plugin")
	w := postJSON(r, "/api/items/item-plugin/favorite", map[string]interface{}{})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var item models.CapabilityItem
	if err := database.DB.First(&item, "id = ?", "item-plugin").Error; err != nil {
		t.Fatalf("failed to load item: %v", err)
	}
	if item.FavoriteCount != 1 {
		t.Errorf("expected favorite_count=1, got %d", item.FavoriteCount)
	}
	var count int64
	database.DB.Model(&models.ItemFavorite{}).Where("item_id = ? AND user_id = ?", "item-plugin", "user-plugin").Count(&count)
	if count != 1 {
		t.Errorf("expected 1 ItemFavorite row, got %d", count)
	}
}

func TestUnfavoriteItem_Plugin(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{
		ID: "repo-plugin2", Name: "repo-plugin2", OwnerID: "u1", Visibility: "public",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-plugin2", Name: "plugin-reg2", SourceType: "internal", RepoID: "repo-plugin2", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-plugin2", RegistryID: "reg-plugin2", RepoID: "repo-plugin2", Slug: "demo-plugin2", ItemType: "plugin",
		Name: "Demo Plugin 2", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
		FavoriteCount: 1,
	})
	database.DB.Create(&models.ItemFavorite{
		ID: "fav-plugin", ItemID: "item-plugin2", UserID: "user-plugin2",
	})

	r := newMarketplaceRouter("user-plugin2")
	w := deleteReq(r, "/api/items/item-plugin2/favorite")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var item models.CapabilityItem
	if err := database.DB.First(&item, "id = ?", "item-plugin2").Error; err != nil {
		t.Fatalf("failed to load item: %v", err)
	}
	if item.FavoriteCount != 0 {
		t.Errorf("expected favorite_count=0, got %d", item.FavoriteCount)
	}
}
