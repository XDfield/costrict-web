package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

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

	// Setup services with mock implementations
	db := database.GetDB()
	embeddingSvc := services.NewEmbeddingService(&config.EmbeddingConfig{
		Provider:   "mock",
		Dimensions: 1024,
	})
	searchSvc := services.NewSearchService(db, embeddingSvc, &config.SearchConfig{})
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
		ID:          "item-rec-skill",
		RegistryID:  "reg-rec2",
		Slug:        "skill-type",
		ItemType:    "skill",
		Name:        "Skill Type",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-rec-cmd",
		RegistryID:  "reg-rec2",
		Slug:        "cmd-type",
		ItemType:    "command",
		Name:        "Command Type",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
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
			ID:          "item-trend" + string(rune('a'+i)),
			RegistryID:  "reg-trend2",
			Slug:        "trending-skill-" + string(rune('a'+i)),
			ItemType:    "skill",
			Name:        "Trending Skill",
			Status:      "active",
			CreatedBy:   "u1",
			Metadata:    datatypes.JSON([]byte("{}")),
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
			ID:          "item-new" + string(rune('b'+i)),
			RegistryID:  "reg-new2",
			Slug:        "new-skill-" + string(rune('b'+i)),
			ItemType:    "skill",
			Name:        "New Skill",
			Status:      "active",
			CreatedBy:   "u1",
			Metadata:    datatypes.JSON([]byte("{}")),
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
	embedding := "[0.1,0.2,0.3]"
	embedding2 := "[0.2,0.3,0.4]"
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
		Embedding:   &embedding,
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
		Embedding:   &embedding2,
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
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", w.Code)
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
		ID:          "item-behavior",
		RegistryID:  "reg-behavior",
		Slug:        "behavior-skill",
		ItemType:    "skill",
		Name:        "Behavior Skill",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
	})

	r := newMarketplaceRouter("")
	w := postJSON(r, "/api/items/item-behavior/behavior", map[string]interface{}{
		"actionType": "view",
		"durationMs": 5000,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func TestLogBehavior_MissingActionType(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-beh2", Name: "beh-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-beh2",
		RegistryID:  "reg-beh2",
		Slug:        "beh-skill",
		ItemType:    "skill",
		Name:        "Beh Skill",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
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

// ---------------------------------------------------------------------------
// GetItemStats
// ---------------------------------------------------------------------------

func TestGetItemStats_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-stats", Name: "stats-reg", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-stats",
		RegistryID:  "reg-stats",
		Slug:        "stats-skill",
		ItemType:    "skill",
		Name:        "Stats Skill",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
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
}

func TestGetItemStats_NoData(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-stats2", Name: "stats-reg2", SourceType: "internal", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID:          "item-stats2",
		RegistryID:  "reg-stats2",
		Slug:        "stats-skill2",
		ItemType:    "skill",
		Name:        "Stats Skill 2",
		Status:      "active",
		CreatedBy:   "u1",
		Metadata:    datatypes.JSON([]byte("{}")),
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
