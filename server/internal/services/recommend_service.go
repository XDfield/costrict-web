package services

import (
	"context"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// RecommendService provides multi-strategy recommendations
type RecommendService struct {
	db           *gorm.DB
	behaviorSvc  *BehaviorService
	searchSvc    *SearchService
}

// NewRecommendService creates a new recommend service
func NewRecommendService(db *gorm.DB, behaviorSvc *BehaviorService, searchSvc *SearchService) *RecommendService {
	return &RecommendService{
		db:          db,
		behaviorSvc: behaviorSvc,
		searchSvc:   searchSvc,
	}
}

// RecommendRequest represents a recommendation request
type RecommendRequest struct {
	UserID       string   `json:"userId"`
	Page         int      `json:"page"`
	PageSize     int      `json:"pageSize"`
	Types        []string `json:"types"`
	Categories   []string `json:"categories"`
	Context      string   `json:"context"`      // Context hint for recommendations
	SessionItems []string `json:"sessionItems"` // Items viewed in current session
}

// RecommendResponse represents a recommendation response
type RecommendResponse struct {
	Items       []RecommendedItem `json:"items"`
	Total       int64             `json:"total"`
	Page        int               `json:"page"`
	PageSize    int               `json:"pageSize"`
	HasMore     bool              `json:"hasMore"`
	Strategies  []string          `json:"strategies"`
	GeneratedAt time.Time         `json:"generatedAt"`
}

// RecommendedItem represents a recommended item with metadata
type RecommendedItem struct {
	models.CapabilityItem
	Score     float64 `json:"score"`
	Reason    string  `json:"reason"`
	Strategy  string  `json:"strategy"`
}

// GetRecommendations returns personalized recommendations using multiple strategies
func (s *RecommendService) GetRecommendations(ctx context.Context, req RecommendRequest) (*RecommendResponse, error) {
	if req.Page <= 0 {
		req.Page = 1
	}
	if req.PageSize <= 0 {
		req.PageSize = 10
	}

	// Collect recommendations from multiple strategies
	var allItems []RecommendedItem
	var strategies []string

	// Strategy 1: Collaborative filtering (similar users)
	if items, err := s.collaborativeFiltering(ctx, req); err == nil && len(items) > 0 {
		allItems = append(allItems, items...)
		strategies = append(strategies, "collaborative_filtering")
	}

	// Strategy 2: Content-based (based on user's history)
	if items, err := s.contentBasedFiltering(ctx, req); err == nil && len(items) > 0 {
		allItems = append(allItems, items...)
		strategies = append(strategies, "content_based")
	}

	// Strategy 3: Popularity-based
	if items, err := s.popularityBased(ctx, req); err == nil && len(items) > 0 {
		allItems = append(allItems, items...)
		strategies = append(strategies, "popularity")
	}

	// Strategy 4: Context-based (related to session items)
	if len(req.SessionItems) > 0 {
		if items, err := s.contextBased(ctx, req); err == nil && len(items) > 0 {
			allItems = append(allItems, items...)
			strategies = append(strategies, "context_based")
		}
	}

	// Deduplicate all items (no limit)
	allDeduped := s.rankAndDedupeAll(allItems)

	// Paginate
	total := int64(len(allDeduped))
	start := (req.Page - 1) * req.PageSize
	end := start + req.PageSize
	if start > len(allDeduped) {
		start = len(allDeduped)
	}
	if end > len(allDeduped) {
		end = len(allDeduped)
	}
	finalItems := allDeduped[start:end]

	return &RecommendResponse{
		Items:       finalItems,
		Total:       total,
		Page:        req.Page,
		PageSize:    req.PageSize,
		HasMore:     int64(end) < total,
		Strategies:  strategies,
		GeneratedAt: time.Now(),
	}, nil
}

// collaborativeFiltering recommends items based on similar users
func (s *RecommendService) collaborativeFiltering(ctx context.Context, req RecommendRequest) ([]RecommendedItem, error) {
	// Find users with similar behavior patterns
	var similarUserIDs []string
	s.db.Model(&models.BehaviorLog{}).
		Select("user_id").
		Where("user_id != ? AND item_id IN (SELECT item_id FROM behavior_logs WHERE user_id = ?)",
			req.UserID, req.UserID).
		Group("user_id").
		Having("COUNT(DISTINCT item_id) >= 2").
		Order("COUNT(*) DESC").
		Limit(20).
		Pluck("user_id", &similarUserIDs)

	if len(similarUserIDs) == 0 {
		return nil, nil
	}

	// Get items popular among similar users but not used by current user
	query := s.db.Model(&models.CapabilityItem{}).
		Joins("JOIN behavior_logs bl ON bl.item_id = capability_items.id").
		Where("bl.user_id IN ?", similarUserIDs).
		Where("capability_items.id NOT IN (SELECT item_id FROM behavior_logs WHERE user_id = ?)",
			req.UserID).
		Where("capability_items.status = ?", "active").
		Group("capability_items.id").
		Order("COUNT(*) DESC").
		Limit(req.PageSize)

	if len(req.Types) > 0 {
		query = query.Where("capability_items.item_type IN ?", req.Types)
	}
	if len(req.Categories) > 0 {
		query = query.Where("capability_items.category IN ?", req.Categories)
	}

	var items []models.CapabilityItem
	if result := query.Find(&items); result.Error != nil {
		return nil, result.Error
	}

	result := make([]RecommendedItem, len(items))
	for i, item := range items {
		result[i] = RecommendedItem{
			CapabilityItem: item,
			Score:          0.8,
			Reason:         "Users with similar interests also used this",
			Strategy:       "collaborative_filtering",
		}
	}

	return result, nil
}

// contentBasedFiltering recommends items similar to user's history
func (s *RecommendService) contentBasedFiltering(ctx context.Context, req RecommendRequest) ([]RecommendedItem, error) {
	// Get user's frequently used categories and types
	var favoriteCategories []string
	s.db.Model(&models.BehaviorLog{}).
		Select("ci.category").
		Joins("JOIN capability_items ci ON ci.id = behavior_logs.item_id").
		Where("behavior_logs.user_id = ? AND ci.category != ''", req.UserID).
		Group("ci.category").
		Order("COUNT(*) DESC").
		Limit(5).
		Pluck("ci.category", &favoriteCategories)

	if len(favoriteCategories) == 0 {
		return nil, nil
	}

	// Get items in favorite categories not yet used by user
	query := s.db.Model(&models.CapabilityItem{}).
		Where("category IN ?", favoriteCategories).
		Where("id NOT IN (SELECT item_id FROM behavior_logs WHERE user_id = ?)", req.UserID).
		Where("status = ?", "active").
		Order("experience_score DESC, install_count DESC").
		Limit(req.PageSize)

	if len(req.Types) > 0 {
		query = query.Where("item_type IN ?", req.Types)
	}

	var items []models.CapabilityItem
	if result := query.Find(&items); result.Error != nil {
		return nil, result.Error
	}

	result := make([]RecommendedItem, len(items))
	for i, item := range items {
		result[i] = RecommendedItem{
			CapabilityItem: item,
			Score:          0.7,
			Reason:         "Based on your interests in " + item.Category,
			Strategy:       "content_based",
		}
	}

	return result, nil
}

// popularityBased recommends popular items
func (s *RecommendService) popularityBased(ctx context.Context, req RecommendRequest) ([]RecommendedItem, error) {
	// Get recently popular items (high install count in last 30 days)
	thirtyDaysAgo := time.Now().AddDate(0, 0, -30)

	query := s.db.Model(&models.CapabilityItem{}).
		Select("capability_items.*, COUNT(bl.id) as recent_installs").
		Joins("LEFT JOIN behavior_logs bl ON bl.item_id = capability_items.id AND bl.action_type = 'install' AND bl.created_at > ?", thirtyDaysAgo).
		Where("capability_items.status = ?", "active").
		Group("capability_items.id").
		Order("recent_installs DESC, capability_items.install_count DESC").
		Limit(req.PageSize)

	if len(req.Types) > 0 {
		query = query.Where("capability_items.item_type IN ?", req.Types)
	}
	if len(req.Categories) > 0 {
		query = query.Where("capability_items.category IN ?", req.Categories)
	}

	var items []models.CapabilityItem
	if result := query.Find(&items); result.Error != nil {
		return nil, result.Error
	}

	result := make([]RecommendedItem, len(items))
	for i, item := range items {
		result[i] = RecommendedItem{
			CapabilityItem: item,
			Score:          0.6,
			Reason:         "Popular in the community",
			Strategy:       "popularity",
		}
	}

	return result, nil
}

// contextBased recommends items related to current session
func (s *RecommendService) contextBased(ctx context.Context, req RecommendRequest) ([]RecommendedItem, error) {
	// Get categories from session items
	var sessionCategories []string
	s.db.Model(&models.CapabilityItem{}).
		Where("id IN ? AND category != ''", req.SessionItems).
		Distinct("category").
		Pluck("category", &sessionCategories)

	if len(sessionCategories) == 0 {
		return nil, nil
	}

	// Get related items
	query := s.db.Model(&models.CapabilityItem{}).
		Where("category IN ?", sessionCategories).
		Where("id NOT IN ?", req.SessionItems).
		Where("status = ?", "active").
		Order("experience_score DESC").
		Limit(req.PageSize)

	if len(req.Types) > 0 {
		query = query.Where("item_type IN ?", req.Types)
	}

	var items []models.CapabilityItem
	if result := query.Find(&items); result.Error != nil {
		return nil, result.Error
	}

	result := make([]RecommendedItem, len(items))
	for i, item := range items {
		result[i] = RecommendedItem{
			CapabilityItem: item,
			Score:          0.75,
			Reason:         "Related to what you're viewing",
			Strategy:       "context_based",
		}
	}

	return result, nil
}

// rankAndDedupeAll deduplicates recommendations without limit
func (s *RecommendService) rankAndDedupeAll(items []RecommendedItem) []RecommendedItem {
	seen := make(map[string]bool)
	result := make([]RecommendedItem, 0, len(items))

	for _, item := range items {
		if seen[item.ID] {
			continue
		}
		seen[item.ID] = true
		result = append(result, item)
	}

	return result
}

// GetTrendingItems returns trending items
func (s *RecommendService) GetTrendingItems(ctx context.Context, page, pageSize int, itemTypes []string) ([]models.CapabilityItem, int64, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}

	sevenDaysAgo := time.Now().AddDate(0, 0, -7)

	var total int64
	countQuery := database.GetDB().Model(&models.CapabilityItem{}).
		Where("capability_items.status = ?", "active")
	if len(itemTypes) > 0 {
		countQuery = countQuery.Where("capability_items.item_type IN ?", itemTypes)
	}
	if err := countQuery.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	query := database.GetDB().Model(&models.CapabilityItem{}).
		Select("capability_items.*, COUNT(bl.id) as recent_activity").
		Joins("LEFT JOIN behavior_logs bl ON bl.item_id = capability_items.id AND bl.created_at > ?", sevenDaysAgo).
		Where("capability_items.status = ?", "active").
		Group("capability_items.id").
		Order("recent_activity DESC, capability_items.install_count DESC").
		Offset((page - 1) * pageSize).Limit(pageSize)

	if len(itemTypes) > 0 {
		query = query.Where("capability_items.item_type IN ?", itemTypes)
	}

	var items []models.CapabilityItem
	if result := query.Find(&items); result.Error != nil {
		return nil, 0, result.Error
	}

	return items, total, nil
}

// GetNewAndNoteworthy returns recently added high-quality items
func (s *RecommendService) GetNewAndNoteworthy(ctx context.Context, page, pageSize int) ([]models.CapabilityItem, int64, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 10
	}

	thirtyDaysAgo := time.Now().AddDate(0, 0, -30)

	var total int64
	countQuery := database.GetDB().Model(&models.CapabilityItem{}).
		Where("status = ? AND created_at > ?", "active", thirtyDaysAgo)
	if err := countQuery.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var items []models.CapabilityItem
	result := database.GetDB().Model(&models.CapabilityItem{}).
		Where("status = ? AND created_at > ?", "active", thirtyDaysAgo).
		Order("experience_score DESC, install_count DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&items)

	if result.Error != nil {
		return nil, 0, result.Error
	}

	return items, total, nil
}
