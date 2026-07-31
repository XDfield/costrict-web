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

	// Behavior logs are append-only. Per-user feedback deduplication (one vote per
	// user) is applied at READ time in GetItemBehaviorStats (latest rating/feedback
	// per user), not by mutating the log — that avoids losing a prior rating on a
	// text-only edit, avoids non-atomic delete-then-insert data loss, and is
	// race-free under concurrent resubmits (SRC-2026-4791 P1-1).
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

	// Keep the denormalized counters in sync. Only actions that actually move a
	// counter go through updateItemStats — feedback/click/use/ignore have no
	// aggregate, so skip the otherwise no-op call (and its goroutine).
	if req.ItemID != "" && countsTowardItemStats(req.ActionType) {
		if s.db.Dialector.Name() == "postgres" {
			go s.updateItemStats(req.ItemID, req.ActionType, userID)
		} else {
			s.updateItemStats(req.ItemID, req.ActionType, userID)
		}
	}

	return log, nil
}

// countsTowardItemStats reports whether an action moves a denormalized counter
// or the experience score. Other actions are logged but never reach updateItemStats.
func countsTowardItemStats(a models.ActionType) bool {
	switch a {
	case models.ActionView, models.ActionInstall, models.ActionSuccess, models.ActionFail:
		return true
	default:
		return false
	}
}

// isFirstUserAction reports whether the just-logged row is this user's FIRST
// occurrence of actionType on itemID. LogBehavior inserts the log row before
// calling updateItemStats, so a count of exactly 1 means "first time". This keeps
// install_count / preview_count as distinct-user counts with a cheap, bounded
// query (the composite index idx_behavior_item_action_user makes it an index-only
// scan of just this user's rows for the item), instead of recomputing
// COUNT(DISTINCT) over every viewer on each write (SRC-2026-4791 P1-1).
//
// Concurrency: two in-flight actions from the same user can race and produce a
// rare off-by-one (both see 2 rows, both skip) — accepted for a denormalized
// counter and corrected by the P2 recompute/backfill.
func (s *BehaviorService) isFirstUserAction(itemID, userID string, action models.ActionType) bool {
	var n int64
	if err := s.db.Model(&models.BehaviorLog{}).
		Where("item_id = ? AND user_id = ? AND action_type = ?", itemID, userID, action).
		Count(&n).Error; err != nil {
		// Can't tell if this is the first action — skip the bump (avoid over-count)
		// but log it so the undercount is observable.
		logger.Warn("[behavior] first-action count failed item=%s user=%s action=%s: %v", itemID, userID, action, err)
		return false
	}
	return n == 1
}

// updateItemStats updates item statistics based on behavior.
//
// SRC-2026-4791: the denormalized counters (preview_count, install_count) and
// experience_score are publicly exposed and drive item-list sorting and
// trending/popularity ranking. They are kept as DISTINCT-user counts (a single
// account can't self-inflate them). Anonymous actions never reach the counter
// updates (guarded below) — they are still logged for raw telemetry.
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
		// preview_count = distinct authenticated viewers: only the user's first view
		// bumps it, so reloading a page (or one account) can't inflate it.
		if s.isFirstUserAction(itemID, userID, models.ActionView) {
			if err := db.Model(&models.CapabilityItem{}).
				Where("id = ?", itemID).
				UpdateColumn("preview_count", gorm.Expr("preview_count + 1")).Error; err != nil {
				logger.Warn("[behavior] update preview_count failed item=%s: %v", itemID, err)
			}
		}

	case models.ActionInstall:
		// install_count = distinct authenticated installers: only the user's first
		// install bumps it, so one account (or repeated /download hits) can't pump
		// the ranking signal.
		if s.isFirstUserAction(itemID, userID, models.ActionInstall) {
			if err := db.Model(&models.CapabilityItem{}).
				Where("id = ?", itemID).
				UpdateColumn("install_count", gorm.Expr("install_count + 1")).Error; err != nil {
				logger.Warn("[behavior] update install_count failed item=%s: %v", itemID, err)
			}
		}

	case models.ActionSuccess, models.ActionFail:
		s.updateExperienceScore(itemID)
	}
}

// FavoriteItem marks an item as favorited for the user (idempotent) and persists
// the per-user invokeMode preference. invokeMode is "auto" or "manual"; an empty
// value defaults to "auto". For an already-favorited item this upserts the mode
// (so the user can switch auto/manual without unfavorite+refavorite).
func (s *BehaviorService) FavoriteItem(ctx context.Context, itemID, userID, invokeMode string) (int64, bool, error) {
	if invokeMode != "manual" {
		invokeMode = "auto"
	}

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

	count, created, err := s.favoriteItemTx(tx, itemID, userID, invokeMode)
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
// status change (and consistent with UnfavoriteItemTx below). Distributed
// favorites default to "auto" (AI-auto-invokable), matching prior behavior.
func (s *BehaviorService) FavoriteItemTx(tx *gorm.DB, itemID, userID string) (int64, bool, error) {
	return s.favoriteItemTx(tx, itemID, userID, "auto")
}

func (s *BehaviorService) favoriteItemTx(tx *gorm.DB, itemID, userID, invokeMode string) (int64, bool, error) {
	favorite := models.ItemFavorite{
		ID:         uuid.New().String(),
		ItemID:     itemID,
		UserID:     userID,
		InvokeMode: invokeMode,
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
	} else if existing.InvokeMode != invokeMode {
		// Already favorited: update the per-user invoke mode in place (count unchanged).
		// No tx.Rollback here — this helper runs inside the caller's tx; the caller
		// (FavoriteItem / the distribution path) owns rollback on a returned error.
		if err := tx.Model(&models.ItemFavorite{}).
			Where("id = ?", existing.ID).
			UpdateColumn("invoke_mode", invokeMode).Error; err != nil {
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

	// Success rate by DISTINCT users, so it matches GetItemBehaviorStats.SuccessRate
	// (which also counts distinct users) and one account can't skew the score by
	// repeating success/fail. Exclude anonymous rows; COALESCE treats a NULL user_id
	// (legacy/unattributable) the same as the anonymous sentinel — untrusted, so
	// excluded — rather than letting SQL three-valued logic drop it implicitly
	// (SRC-2026-4791 P1-1).
	var rows []struct {
		ActionType models.ActionType
		Count      int64
	}
	db.Model(&models.BehaviorLog{}).
		Select("action_type, COUNT(DISTINCT user_id) as count").
		Where("item_id = ? AND COALESCE(user_id, ?) <> ? AND action_type IN ?", itemID, models.AnonymousUserID, models.AnonymousUserID, []models.ActionType{models.ActionSuccess, models.ActionFail}).
		Group("action_type").
		Scan(&rows)

	var successUsers, failUsers int64
	for _, r := range rows {
		switch r.ActionType {
		case models.ActionSuccess:
			successUsers = r.Count
		case models.ActionFail:
			failUsers = r.Count
		}
	}

	// Always write the recomputed score (0 when there is no trusted data) so an
	// item whose only success/fail history was anonymous is corrected down rather
	// than keeping a previously inflated value.
	score := 0.0
	if total := successUsers + failUsers; total > 0 {
		score = float64(successUsers) / float64(total)
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

	// Count DISTINCT authenticated users per action type. Excluding anonymous/
	// unattributable rows keeps unauthenticated (or historically injected) writes
	// out of the public stats, and counting distinct users (not raw events) matches
	// the denormalized preview_count/install_count — which P1-1 also made
	// distinct-user — so the item card and the stats panel agree (SRC-2026-4791).
	actionCounts := make(map[models.ActionType]int64)
	var results []struct {
		ActionType models.ActionType
		Count      int64
	}
	s.db.Model(&models.BehaviorLog{}).
		Select("action_type, COUNT(DISTINCT user_id) as count").
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

	// Average rating — one vote per user (the user's latest rating), computed at
	// read time so the append-only log needs no supersede: a text-only edit can't
	// erase an earlier star rating and concurrent resubmits can't double-count
	// (SRC-2026-4791 P1-1). Anonymous/NULL rows excluded.
	//
	// "latest" = ORDER BY created_at DESC, id DESC. The id tiebreaker (random UUID)
	// only matters when two of the SAME user's rows share an identical created_at —
	// which needs two submits within one timestamp tick. Sequential human submits
	// are milliseconds+ apart (timestamptz is microsecond-precise in Postgres), so
	// this is a non-issue in practice; accepted rather than adding a sequence column.
	s.db.Raw(`
		SELECT COALESCE(AVG(rating), 0) FROM (
			SELECT rating,
			       ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY created_at DESC, id DESC) AS rn
			FROM behavior_logs
			WHERE item_id = ? AND COALESCE(user_id, ?) <> ? AND rating > 0
		) t WHERE rn = 1
	`, itemID, models.AnonymousUserID, models.AnonymousUserID).Scan(&stats.AverageRating)

	// Recent feedback — each user's latest non-empty feedback, 10 most recent.
	s.db.Raw(`
		SELECT feedback FROM (
			SELECT feedback, created_at,
			       ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY created_at DESC, id DESC) AS rn
			FROM behavior_logs
			WHERE item_id = ? AND COALESCE(user_id, ?) <> ? AND feedback <> ''
		) t WHERE rn = 1 ORDER BY created_at DESC LIMIT 10
	`, itemID, models.AnonymousUserID, models.AnonymousUserID).Scan(&stats.RecentFeedback)

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
