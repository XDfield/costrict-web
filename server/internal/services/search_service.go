package services

import (
	"context"
	"fmt"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// SearchService provides search capabilities
type SearchService struct {
	db  *gorm.DB
	cfg *config.SearchConfig
}

// NewSearchService creates a new search service
func NewSearchService(db *gorm.DB, cfg *config.SearchConfig) *SearchService {
	return &SearchService{
		db:  db,
		cfg: cfg,
	}
}

// SearchRequest represents a search request
type SearchRequest struct {
	Query       string   `json:"query"`
	Page        int      `json:"page"`
	PageSize    int      `json:"pageSize"`
	Types       []string `json:"types"`
	Categories  []string `json:"categories"`
	RegistryIDs []string `json:"registryIds"`
	MinScore    float64  `json:"minScore"`
}

// SearchResult represents a search result
type SearchResult struct {
	Items    []SearchResultItem `json:"items"`
	Total    int64              `json:"total"`
	Query    string             `json:"query"`
	Duration int64              `json:"durationMs"`
}

// SearchResultItem represents an item in search results
type SearchResultItem struct {
	models.CapabilityItem
	Score float64 `json:"score"`
}

// SemanticSearch previously performed vector similarity search. pgvector has been
// removed, so this now delegates to keyword search.
func (s *SearchService) SemanticSearch(ctx context.Context, req SearchRequest) (*SearchResult, error) {
	_ = ctx
	return s.KeywordSearch(req)
}

// HybridSearch previously combined semantic and keyword search. pgvector has been
// removed, so this now delegates to keyword search.
func (s *SearchService) HybridSearch(ctx context.Context, req SearchRequest) (*SearchResult, error) {
	_ = ctx
	return s.KeywordSearch(req)
}

// KeywordSearch performs traditional keyword search
func (s *SearchService) KeywordSearch(req SearchRequest) (*SearchResult, error) {
	if req.PageSize <= 0 {
		req.PageSize = s.cfg.DefaultLimit
	}
	if req.Page <= 0 {
		req.Page = 1
	}

	query := s.db.Model(&models.CapabilityItem{}).Where("status = ?", "active")

	like := database.ILike(s.db)
	for _, kw := range database.SplitSearchKeywords(req.Query) {
		pattern := "%" + kw + "%"
		query = query.Where(fmt.Sprintf("name %s ? OR description %s ? OR content %s ?", like, like, like),
			pattern, pattern, pattern)
	}

	if len(req.Types) > 0 {
		query = query.Where("item_type IN ?", req.Types)
	}
	if len(req.Categories) > 0 {
		query = query.Where("category IN ?", req.Categories)
	}
	if len(req.RegistryIDs) > 0 {
		query = query.Where("registry_id IN ?", req.RegistryIDs)
	}

	var total int64
	query.Count(&total)

	var items []models.CapabilityItem
	result := query.Order("created_at DESC").Limit(req.PageSize).Offset((req.Page - 1) * req.PageSize).Find(&items)
	if result.Error != nil {
		return nil, fmt.Errorf("failed to search items: %w", result.Error)
	}

	// Convert to search results with score
	searchItems := make([]SearchResultItem, len(items))
	for i, item := range items {
		searchItems[i] = SearchResultItem{
			CapabilityItem: item,
			Score:          1.0, // Default score for keyword search
		}
	}

	return &SearchResult{
		Items: searchItems,
		Total: total,
		Query: req.Query,
	}, nil
}

// FindSimilar previously used vector similarity to find related items. pgvector
// has been removed, so it now returns an empty result set.
func (s *SearchService) FindSimilar(ctx context.Context, itemID string, page, pageSize int) ([]SearchResultItem, int64, error) {
	_ = ctx
	_ = itemID
	_ = page
	_ = pageSize
	return []SearchResultItem{}, 0, nil
}
