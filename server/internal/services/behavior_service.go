package services

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// BehaviorService handles user behavior tracking
type BehaviorService struct {
	db *gorm.DB
}

// NewBehaviorService creates a new behavior service
func NewBehaviorService(db *gorm.DB) *BehaviorService {
	return &BehaviorService{db: db}
}

// ErrSkillRequired is returned by the unfavorite path when the item is still
// required by an active readonly distribution. It is an EXPECTED outcome (keep
// the favorite), not a failure — callers running inside a transaction (the
// distribution revoke/pause path) tolerate it while still propagating real
// DB errors so the surrounding status change can roll back.
var ErrSkillRequired = errors.New("cannot unfavorite a required skill")

// LogBehaviorRequest represents a behavior log request
type LogBehaviorRequest struct {
	UserID      string                 `json:"userId"`
	ItemID      string                 `json:"itemId"`
	RegistryID  string                 `json:"registryId"`
	ActionType  models.ActionType      `json:"actionType" binding:"required"`
	Context     models.ContextType     `json:"context"`
	SearchQuery string                 `json:"searchQuery"`
	SessionID   string                 `json:"sessionId"`
	DurationMs  int64                  `json:"durationMs"`
	Rating      int                    `json:"rating"`
	Feedback    string                 `json:"feedback"`
	Metadata    map[string]interface{} `json:"metadata"`
}

// LogBehavior logs a user behavior
func (s *BehaviorService) LogBehavior(ctx context.Context, req LogBehaviorRequest) (*models.BehaviorLog, error) {
	// Build metadata
	var metadataJSON datatypes.JSON
	if req.Metadata != nil {
		data, _ := json.Marshal(req.Metadata)
		metadataJSON = datatypes.JSON(data)
	} else {
		metadataJSON = datatypes.JSON([]byte("{}"))
	}

	// Handle empty strings for UUID fields - convert to valid format or skip
	userID := req.UserID
	if userID == "" {
		userID = models.AnonymousUserID // Use a placeholder for anonymous users
	}

	log := &models.BehaviorLog{
		ID:          uuid.New().String(),
		UserID:      userID,
		ActionType:  req.ActionType,
		Context:     req.Context,
		SearchQuery: req.SearchQuery,
		SessionID:   req.SessionID,
		DurationMs:  req.DurationMs,
		Rating:      req.Rating,
		Feedback:    req.Feedback,
		Metadata:    metadataJSON,
	}

	// PostgreSQL stores UUIDs here, while tests use SQLite/TEXT IDs.
	if req.ItemID != "" {
		if _, err := uuid.Parse(req.ItemID); err == nil || s.db.Dialector.Name() != "postgres" {
			log.ItemID = req.ItemID
		}
	}

	// PostgreSQL stores UUIDs here, while tests use SQLite/TEXT IDs.
	if req.RegistryID != "" {
		if _, err := uuid.Parse(req.RegistryID); err == nil || s.db.Dialector.Name() != "postgres" {
			log.RegistryID = req.RegistryID
		}
	}

	createDB := s.db.WithContext(ctx)
	if log.ItemID == "" {
		createDB = createDB.Omit("ItemID")
	}
	if log.RegistryID == "" {
		createDB = createDB.Omit("RegistryID")
	}

	result := createDB.Create(log)
	if result.Error != nil {
		return nil, result.Error
	}

	// Keep aggregate counters in sync without blocking the request path in production.
	if req.ItemID != "" {
		if s.db.Dialector.Name() == "postgres" {
			go s.updateItemStats(req.ItemID, req.ActionType, userID)
		} else {
			s.updateItemStats(req.ItemID, req.ActionType, userID)
		}
	}

	return log, nil
}

// updateItemStats updates item statistics based on behavior.
//
// SRC-2026-4791: the denormalized counters (preview_count, install_count) and
// experience_score are publicly exposed and drive item-list sorting and
// trending/popularity ranking. Anonymous actions must never move them, or an
// unauthenticated caller could forge them (e.g. flooding anonymous view via
// /behavior or anonymous install via the public /download endpoint) to game
// the ranking. Anonymous rows are still logged for raw telemetry, they just
// don't affect these aggregates.
func (s *BehaviorService) updateItemStats(itemID string, actionType models.ActionType, userID string) {
	db := s.db
	if db == nil {
		logger.Warn("[behavior] skip aggregate update: db is nil item=%s action=%s", itemID, actionType)
		return
	}

	if userID == "" || userID == models.AnonymousUserID {
		return
	}

	switch actionType {
	case models.ActionView:
		if err := db.Model(&models.CapabilityItem{}).
			Where("id = ?", itemID).
			UpdateColumn("preview_count", gorm.Expr("preview_count + 1")).Error; err != nil {
			logger.Warn("[behavior] update preview_count failed item=%s: %v", itemID, err)
		}

	case models.ActionInstall:
		if err := db.Model(&models.CapabilityItem{}).
			Where("id = ?", itemID).
			UpdateColumn("install_count", gorm.Expr("install_count + 1")).Error; err != nil {
			logger.Warn("[behavior] update install_count failed item=%s: %v", itemID, err)
		}

	case models.ActionSuccess, models.ActionFail:
		s.updateExperienceScore(itemID)
	}
}

func (s *BehaviorService) FavoriteItem(ctx context.Context, itemID, userID string) (int64, bool, error) {
	tx := s.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return 0, false, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			panic(r)
		}
	}()

	count, created, err := s.favoriteItemTx(tx, itemID, userID)
	if err != nil {
		tx.Rollback()
		return 0, false, err
	}

	if err := tx.Commit().Error; err != nil {
		return 0, false, err
	}
	return count, created, nil
}

// FavoriteItemTx adds a favorite within the caller's transaction. Used by the
// distribution resume path so re-adding recipients' favorites is atomic with the
// status change (and consistent with UnfavoriteItemTx below).
func (s *BehaviorService) FavoriteItemTx(tx *gorm.DB, itemID, userID string) (int64, bool, error) {
	return s.favoriteItemTx(tx, itemID, userID)
}

func (s *BehaviorService) favoriteItemTx(tx *gorm.DB, itemID, userID string) (int64, bool, error) {
	favorite := models.ItemFavorite{
		ID:     uuid.New().String(),
		ItemID: itemID,
		UserID: userID,
	}
	var existing models.ItemFavorite
	err := tx.Where("item_id = ? AND user_id = ?", itemID, userID).First(&existing).Error
	if err != nil && err != gorm.ErrRecordNotFound {
		return 0, false, err
	}

	created := err == gorm.ErrRecordNotFound
	if created {
		if err := tx.Create(&favorite).Error; err != nil {
			return 0, false, err
		}
		if err := tx.Model(&models.CapabilityItem{}).
			Where("id = ?", itemID).
			UpdateColumn("favorite_count", gorm.Expr("favorite_count + 1")).Error; err != nil {
			return 0, false, err
		}
	}

	var count int64
	if err := tx.Model(&models.CapabilityItem{}).
		Where("id = ?", itemID).
		Select("favorite_count").
		Scan(&count).Error; err != nil {
		return 0, false, err
	}

	return count, created, nil
}

func (s *BehaviorService) UnfavoriteItem(ctx context.Context, itemID, userID string) (int64, bool, error) {
	tx := s.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return 0, false, tx.Error
	}
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			panic(r)
		}
	}()

	count, removed, err := s.unfavoriteItemTx(tx, itemID, userID)
	if err != nil {
		tx.Rollback()
		return 0, false, err
	}

	if err := tx.Commit().Error; err != nil {
		return 0, false, err
	}
	return count, removed, nil
}

// UnfavoriteItemTx removes a favorite within the caller's transaction. The
// distribution revoke/pause path MUST use this (not UnfavoriteItem) so the
// "required skill" guard below evaluates the just-revoked distribution on the
// SAME transaction. With a separate transaction the guard reads the still
// uncommitted distribution status as 'active' and wrongly blocks removing the
// favorite of a revoked/paused readonly distribution — so the recipient keeps a
// favorite that the cloud should no longer report, and /hub never unloads it.
func (s *BehaviorService) UnfavoriteItemTx(tx *gorm.DB, itemID, userID string) (int64, bool, error) {
	return s.unfavoriteItemTx(tx, itemID, userID)
}

func (s *BehaviorService) unfavoriteItemTx(tx *gorm.DB, itemID, userID string) (int64, bool, error) {
	// Prevent unfavoriting items still required by an ACTIVE readonly
	// distribution. A distribution that is already revoked/paused on this tx is
	// (correctly) excluded by status = 'active'.
	var readonlyCount int64
	if err := tx.Model(&models.ItemDistributionReceipt{}).
		Joins("JOIN item_distributions ON item_distributions.id = item_distribution_receipts.distribution_id").
		Where("item_distribution_receipts.user_id = ? AND item_distributions.item_id = ? AND item_distributions.status = ? AND item_distributions.permission_mode = ? AND item_distribution_receipts.receipt_status != ?",
			userID, itemID, "active", "readonly", "dismissed").
		Count(&readonlyCount).Error; err != nil {
		return 0, false, err
	}
	if readonlyCount > 0 {
		return 0, false, ErrSkillRequired
	}

	result := tx.Where("item_id = ? AND user_id = ?", itemID, userID).Delete(&models.ItemFavorite{})
	if result.Error != nil {
		return 0, false, result.Error
	}

	removed := result.RowsAffected > 0
	if removed {
		if err := tx.Model(&models.CapabilityItem{}).
			Where("id = ?", itemID).
			UpdateColumn("favorite_count", gorm.Expr("CASE WHEN favorite_count > 0 THEN favorite_count - 1 ELSE 0 END")).Error; err != nil {
			return 0, false, err
		}
	}

	var count int64
	if err := tx.Model(&models.CapabilityItem{}).
		Where("id = ?", itemID).
		Select("favorite_count").
		Scan(&count).Error; err != nil {
		return 0, false, err
	}

	return count, removed, nil
}

// updateExperienceScore updates the experience score for an item
func (s *BehaviorService) updateExperienceScore(itemID string) {
	db := s.db
	if db == nil {
		logger.Warn("[behavior] skip experience score update: db is nil item=%s", itemID)
		return
	}

	// Calculate success rate. Exclude anonymous rows so unauthenticated writes
	// can't skew the score. COALESCE treats a NULL user_id (legacy/unattributable
	// rows) the same as the anonymous sentinel — untrusted, so excluded — instead
	// of letting SQL three-valued logic drop them implicitly (SRC-2026-4791).
	var total, success int64
	db.Model(&models.BehaviorLog{}).
		Where("item_id = ? AND COALESCE(user_id, ?) <> ? AND action_type IN ?", itemID, models.AnonymousUserID, models.AnonymousUserID, []models.ActionType{models.ActionSuccess, models.ActionFail}).
		Count(&total)

	db.Model(&models.BehaviorLog{}).
		Where("item_id = ? AND COALESCE(user_id, ?) <> ? AND action_type = ?", itemID, models.AnonymousUserID, models.AnonymousUserID, models.ActionSuccess).
		Count(&success)

	// Always write the recomputed score (0 when there is no trusted data) so an
	// item whose only success/fail history was anonymous is corrected down rather
	// than keeping a previously inflated value.
	score := 0.0
	if total > 0 {
		score = float64(success) / float64(total)
	}
	db.Model(&models.CapabilityItem{}).
		Where("id = ?", itemID).
		Update("experience_score", score)
}

// GetUserBehaviorSummary returns a summary of user behavior
func (s *BehaviorService) GetUserBehaviorSummary(ctx context.Context, userID string) (*models.UserBehaviorSummary, error) {
	summary := &models.UserBehaviorSummary{
		UserID: userID,
	}

	// Count views
	s.db.Model(&models.BehaviorLog{}).
		Where("user_id = ? AND action_type = ?", userID, models.ActionView).
		Count(&summary.TotalViews)

	// Count installs
	s.db.Model(&models.BehaviorLog{}).
		Where("user_id = ? AND action_type = ?", userID, models.ActionInstall).
		Count(&summary.TotalInstalls)

	// Count uses
	s.db.Model(&models.BehaviorLog{}).
		Where("user_id = ? AND action_type = ?", userID, models.ActionUse).
		Count(&summary.TotalUses)

	// Calculate success rate
	var total, success int64
	s.db.Model(&models.BehaviorLog{}).
		Where("user_id = ? AND action_type IN ?", userID, []models.ActionType{models.ActionSuccess, models.ActionFail}).
		Count(&total)
	s.db.Model(&models.BehaviorLog{}).
		Where("user_id = ? AND action_type = ?", userID, models.ActionSuccess).
		Count(&success)
	if total > 0 {
		summary.SuccessRate = float64(success) / float64(total)
	}

	// Get favorite types
	s.db.Model(&models.BehaviorLog{}).
		Select("ci.item_type, COUNT(*) as count").
		Joins("JOIN capability_items ci ON ci.id = behavior_logs.item_id").
		Where("behavior_logs.user_id = ?", userID).
		Group("ci.item_type").
		Order("count DESC").
		Limit(5).
		Pluck("ci.item_type", &summary.FavoriteTypes)

	// Get favorite categories
	s.db.Model(&models.BehaviorLog{}).
		Select("ci.category, COUNT(*) as count").
		Joins("JOIN capability_items ci ON ci.id = behavior_logs.item_id").
		Where("behavior_logs.user_id = ? AND ci.category != ''", userID).
		Group("ci.category").
		Order("count DESC").
		Limit(5).
		Pluck("ci.category", &summary.FavoriteCategories)

	return summary, nil
}

// GetItemBehaviorStats returns behavior statistics for an item
func (s *BehaviorService) GetItemBehaviorStats(ctx context.Context, itemID string) (*ItemBehaviorStats, error) {
	stats := &ItemBehaviorStats{ItemID: itemID}

	// Count by action type. Exclude anonymous/unattributable rows so unauthenticated
	// writes (or historically injected ones) can't inflate the public stats. This
	// mirrors the denormalized counters, which also only move for authenticated
	// activity, so stats.Views/Installs stay consistent with previewCount/installCount
	// (SRC-2026-4791).
	actionCounts := make(map[models.ActionType]int64)
	var results []struct {
		ActionType models.ActionType
		Count      int64
	}
	s.db.Model(&models.BehaviorLog{}).
		Select("action_type, COUNT(*) as count").
		Where("item_id = ? AND COALESCE(user_id, ?) <> ?", itemID, models.AnonymousUserID, models.AnonymousUserID).
		Group("action_type").
		Scan(&results)

	for _, r := range results {
		actionCounts[r.ActionType] = r.Count
	}

	stats.Views = actionCounts[models.ActionView]
	stats.Clicks = actionCounts[models.ActionClick]
	stats.Installs = actionCounts[models.ActionInstall]
	s.db.Model(&models.ItemFavorite{}).
		Where("item_id = ?", itemID).
		Count(&stats.Favorites)
	stats.Uses = actionCounts[models.ActionUse]
	stats.Successes = actionCounts[models.ActionSuccess]
	stats.Failures = actionCounts[models.ActionFail]

	// Calculate success rate
	total := stats.Successes + stats.Failures
	if total > 0 {
		stats.SuccessRate = float64(stats.Successes) / float64(total)
	}

	// Average rating (authenticated rows only).
	s.db.Model(&models.BehaviorLog{}).
		Where("item_id = ? AND COALESCE(user_id, ?) <> ? AND rating > 0", itemID, models.AnonymousUserID, models.AnonymousUserID).
		Select("AVG(rating)").
		Scan(&stats.AverageRating)

	// Recent feedback (authenticated rows only).
	s.db.Model(&models.BehaviorLog{}).
		Where("item_id = ? AND COALESCE(user_id, ?) <> ? AND feedback != ''", itemID, models.AnonymousUserID, models.AnonymousUserID).
		Order("created_at DESC").
		Limit(10).
		Pluck("feedback", &stats.RecentFeedback)

	return stats, nil
}

// ItemBehaviorStats contains behavior statistics for an item
type ItemBehaviorStats struct {
	ItemID         string   `json:"itemId"`
	Views          int64    `json:"views"`
	Clicks         int64    `json:"clicks"`
	Installs       int64    `json:"installs"`
	Favorites      int64    `json:"favorites"`
	Uses           int64    `json:"uses"`
	Successes      int64    `json:"successes"`
	Failures       int64    `json:"failures"`
	SuccessRate    float64  `json:"successRate"`
	AverageRating  float64  `json:"averageRating"`
	RecentFeedback []string `json:"recentFeedback"`
}

// GetRecentBehaviors returns recent behaviors for a user
func (s *BehaviorService) GetRecentBehaviors(ctx context.Context, userID string, limit int) ([]models.BehaviorLog, error) {
	if limit <= 0 {
		limit = 50
	}

	var logs []models.BehaviorLog
	result := s.db.Where("user_id = ?", userID).
		Preload("Item").
		Order("created_at DESC").
		Limit(limit).
		Find(&logs)

	if result.Error != nil {
		return nil, result.Error
	}

	return logs, nil
}

// GetBehaviorsByTimeRange returns behaviors within a time range
func (s *BehaviorService) GetBehaviorsByTimeRange(ctx context.Context, startTime, endTime time.Time, itemID string) ([]models.BehaviorLog, error) {
	query := s.db.Where("created_at >= ? AND created_at <= ?", startTime, endTime)

	if itemID != "" {
		query = query.Where("item_id = ?", itemID)
	}

	var logs []models.BehaviorLog
	result := query.Order("created_at ASC").Find(&logs)

	if result.Error != nil {
		return nil, result.Error
	}

	return logs, nil
}
