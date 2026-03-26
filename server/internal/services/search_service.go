package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// SearchService provides semantic search capabilities
type SearchService struct {
	db           *gorm.DB
	embeddingSvc *EmbeddingService
	cfg          *config.SearchConfig
}

// NewSearchService creates a new search service
func NewSearchService(db *gorm.DB, embeddingSvc *EmbeddingService, cfg *config.SearchConfig) *SearchService {
	return &SearchService{
		db:           db,
		embeddingSvc: embeddingSvc,
		cfg:          cfg,
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

// SemanticSearch performs semantic search using vector similarity
func (s *SearchService) SemanticSearch(ctx context.Context, req SearchRequest) (*SearchResult, error) {
	if req.PageSize <= 0 {
		req.PageSize = s.cfg.DefaultLimit
	}
	if req.Page <= 0 {
		req.Page = 1
	}
	if req.MinScore <= 0 {
		req.MinScore = s.cfg.SimilarityThreshold
	}

	// Get embedding for query
	queryEmbedding, err := s.embeddingSvc.GetSingleEmbedding(ctx, req.Query)
	if err != nil {
		// Fall back to keyword search if embedding fails
		return s.KeywordSearch(req)
	}

	// Build vector search query
	vectorStr := FormatVectorForDB(queryEmbedding)

	// Base query for items with embeddings
	query := s.db.Model(&models.CapabilityItem{}).
		Where("embedding IS NOT NULL").
		Where("status = ?", "active")

	// Apply filters
	if len(req.Types) > 0 {
		query = query.Where("item_type IN ?", req.Types)
	}
	if len(req.Categories) > 0 {
		query = query.Where("category IN ?", req.Categories)
	}
	if len(req.RegistryIDs) > 0 {
		query = query.Where("registry_id IN ?", req.RegistryIDs)
	}

	// Count total matching items
	var total int64
	countQuery := s.db.Model(&models.CapabilityItem{}).
		Where("embedding IS NOT NULL").
		Where("status = ?", "active")
	if len(req.Types) > 0 {
		countQuery = countQuery.Where("item_type IN ?", req.Types)
	}
	if len(req.Categories) > 0 {
		countQuery = countQuery.Where("category IN ?", req.Categories)
	}
	if len(req.RegistryIDs) > 0 {
		countQuery = countQuery.Where("registry_id IN ?", req.RegistryIDs)
	}
	countQuery.Count(&total)

	// Perform vector similarity search
	// Using cosine similarity: 1 - (embedding <=> query_vector)
	var items []SearchResultItem

	sql := `
		SELECT * FROM (
			SELECT id, registry_id, slug, item_type, name, description, category, version,
			       content, metadata, source_path, source_sha, install_count,
			       status, created_by, updated_by, created_at, updated_at,
			       embedding_updated_at, experience_score,
			       1 - (embedding <=> ?) as score
			FROM capability_items
			WHERE embedding IS NOT NULL
			  AND status = 'active'
	`

	args := []interface{}{vectorStr}
	whereClauses := []string{}

	if len(req.Types) > 0 {
		placeholders := make([]string, len(req.Types))
		for i, t := range req.Types {
			placeholders[i] = "?"
			args = append(args, t)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("item_type IN (%s)", strings.Join(placeholders, ",")))
	}
	if len(req.Categories) > 0 {
		placeholders := make([]string, len(req.Categories))
		for i, c := range req.Categories {
			placeholders[i] = "?"
			args = append(args, c)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("category IN (%s)", strings.Join(placeholders, ",")))
	}
	if len(req.RegistryIDs) > 0 {
		placeholders := make([]string, len(req.RegistryIDs))
		for i, r := range req.RegistryIDs {
			placeholders[i] = "?"
			args = append(args, r)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("registry_id IN (%s)", strings.Join(placeholders, ",")))
	}

	if len(whereClauses) > 0 {
		sql += " AND " + strings.Join(whereClauses, " AND ")
	}

	sql += ` ) AS sub WHERE score >= ? ORDER BY score DESC LIMIT ? OFFSET ?`
	args = append(args, req.MinScore, req.PageSize, (req.Page-1)*req.PageSize)

	result := database.GetDB().Raw(sql, args...).Scan(&items)
	if result.Error != nil {
		// Fall back to keyword search if vector search fails (e.g., SQLite without pgvector)
		return s.KeywordSearch(req)
	}

	return &SearchResult{
		Items: items,
		Total: total,
		Query: req.Query,
	}, nil
}

// HybridSearch combines semantic and keyword search
func (s *SearchService) HybridSearch(ctx context.Context, req SearchRequest) (*SearchResult, error) {
	if req.PageSize <= 0 {
		req.PageSize = s.cfg.DefaultLimit
	}
	if req.Page <= 0 {
		req.Page = 1
	}

	// Get embedding for query
	queryEmbedding, err := s.embeddingSvc.GetSingleEmbedding(ctx, req.Query)
	if err != nil {
		// Fall back to keyword search if embedding fails
		return s.KeywordSearch(req)
	}

	vectorStr := FormatVectorForDB(queryEmbedding)

	// Build hybrid search query combining semantic and keyword matching
	var items []SearchResultItem

	like := database.ILike(s.db)
	sql := fmt.Sprintf(`
		SELECT id, registry_id, slug, item_type, name, description, category, version,
		       content, metadata, source_path, source_sha, install_count,
		       status, created_by, updated_by, created_at, updated_at,
		       embedding_updated_at, experience_score,
		       COALESCE(1 - (embedding <=> ?), 0) * 0.7 +
		       CASE WHEN name %s ? THEN 0.2 ELSE 0 END +
		       CASE WHEN description %s ? THEN 0.1 ELSE 0 END as score
		FROM capability_items
		WHERE status = 'active'
	`, like, like)

	searchPattern := "%" + req.Query + "%"
	args := []interface{}{vectorStr, searchPattern, searchPattern}

	whereClauses := []string{}
	if len(req.Types) > 0 {
		placeholders := make([]string, len(req.Types))
		for i, t := range req.Types {
			placeholders[i] = "?"
			args = append(args, t)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("item_type IN (%s)", strings.Join(placeholders, ",")))
	}
	if len(req.Categories) > 0 {
		placeholders := make([]string, len(req.Categories))
		for i, c := range req.Categories {
			placeholders[i] = "?"
			args = append(args, c)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("category IN (%s)", strings.Join(placeholders, ",")))
	}
	if len(req.RegistryIDs) > 0 {
		placeholders := make([]string, len(req.RegistryIDs))
		for i, r := range req.RegistryIDs {
			placeholders[i] = "?"
			args = append(args, r)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("registry_id IN (%s)", strings.Join(placeholders, ",")))
	}

	if len(whereClauses) > 0 {
		sql += " AND " + strings.Join(whereClauses, " AND ")
	}

	sql += " ORDER BY score DESC LIMIT ? OFFSET ?"
	args = append(args, req.PageSize, (req.Page-1)*req.PageSize)

	result := database.GetDB().Raw(sql, args...).Scan(&items)
	if result.Error != nil {
		// Fall back to keyword search if vector search fails (e.g., SQLite without pgvector)
		return s.KeywordSearch(req)
	}

	// Count total
	var total int64
	countQuery := s.db.Model(&models.CapabilityItem{}).Where("status = ?", "active")
	if len(req.Types) > 0 {
		countQuery = countQuery.Where("item_type IN ?", req.Types)
	}
	if len(req.Categories) > 0 {
		countQuery = countQuery.Where("category IN ?", req.Categories)
	}
	if len(req.RegistryIDs) > 0 {
		countQuery = countQuery.Where("registry_id IN ?", req.RegistryIDs)
	}
	countQuery = countQuery.Where(fmt.Sprintf("name %s ? OR description %s ?", like, like), searchPattern, searchPattern)
	countQuery.Count(&total)

	return &SearchResult{
		Items: items,
		Total: total,
		Query: req.Query,
	}, nil
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
	searchPattern := "%" + req.Query + "%"
	query = query.Where(fmt.Sprintf("name %s ? OR description %s ? OR content %s ?", like, like, like),
		searchPattern, searchPattern, searchPattern)

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

// FindSimilar finds items similar to a given item
func (s *SearchService) FindSimilar(ctx context.Context, itemID string, page, pageSize int) ([]SearchResultItem, int64, error) {
	if pageSize <= 0 {
		pageSize = 10
	}
	if page <= 0 {
		page = 1
	}

	// Get the item's embedding
	var item models.CapabilityItem
	result := s.db.First(&item, "id = ?", itemID)
	if result.Error != nil {
		return nil, 0, fmt.Errorf("item not found: %w", result.Error)
	}

	if item.Embedding == nil || *item.Embedding == "" || *item.Embedding == "[]" {
		return nil, 0, fmt.Errorf("item has no embedding")
	}

	// Count total similar items
	var total int64
	countResult := database.GetDB().Raw(`
		SELECT COUNT(*) FROM capability_items
		WHERE embedding IS NOT NULL AND status = 'active' AND id != ?
	`, itemID).Scan(&total)
	if countResult.Error != nil {
		total = 0
	}

	// Find similar items
	var similarItems []SearchResultItem
	sql := `
		SELECT id, registry_id, slug, item_type, name, description, category, version,
		       content, metadata, source_path, source_sha, install_count,
		       status, created_by, updated_by, created_at, updated_at,
		       embedding_updated_at, experience_score,
		       1 - (embedding <=> ?) as score
		FROM capability_items
		WHERE embedding IS NOT NULL
		  AND status = 'active'
		  AND id != ?
		ORDER BY score DESC
		LIMIT ?
		OFFSET ?
	`

	result = database.GetDB().Raw(sql, *item.Embedding, itemID, pageSize, (page-1)*pageSize).Scan(&similarItems)
	if result.Error != nil {
		// Return empty list if vector search fails (e.g., SQLite without pgvector)
		return []SearchResultItem{}, 0, nil
	}

	return similarItems, total, nil
}
