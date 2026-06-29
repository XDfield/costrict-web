package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification/sender"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// DistributionService handles item distribution (push/share) logic.
type DistributionService struct {
	db              *gorm.DB
	behaviorSvc     *BehaviorService
	notificationSvc NotificationSender
}

// NotificationSender abstracts the notification service for distribution events.
type NotificationSender interface {
	TriggerMessage(userID, eventType string, msg sender.NotificationMessage)
}

// NewDistributionService creates a new distribution service.
func NewDistributionService(db *gorm.DB, behaviorSvc *BehaviorService) *DistributionService {
	return &DistributionService{db: db, behaviorSvc: behaviorSvc}
}

// SetNotificationService sets the notification service for distribution events.
func (s *DistributionService) SetNotificationService(svc NotificationSender) {
	s.notificationSvc = svc
}

// DistributionTarget represents a single target in a distribute request.
type DistributionTarget struct {
	ScopeType string `json:"scopeType" binding:"required"` // user | organization
	TargetID  string `json:"targetId" binding:"required"`
}

// DistributeItemRequest represents a request to distribute an item.
type DistributeItemRequest struct {
	Targets        []DistributionTarget `json:"targets" binding:"required,min=1"`
	PermissionMode string               `json:"permissionMode" binding:"required,oneof=readonly dismissible"`
	Message        string               `json:"message"`
	ExpiresAt      *time.Time           `json:"expiresAt,omitempty"`
}

// DistributionResult holds the result of distributing to one target.
type DistributionResult struct {
	Distribution *models.ItemDistribution `json:"distribution"`
	RecipientCount int                    `json:"recipientCount"`
}

var (
	ErrNotDistributor    = errors.New("only the distributor or platform admin can modify this distribution")
	ErrDistributionNotFound = errors.New("distribution not found")
	ErrInvalidPermissionMode = errors.New("invalid permission mode")
	ErrCannotDistribute    = errors.New("you do not have permission to push this item")
)

// CanDistribute checks if a user can distribute an item.
// Only platform admins are allowed to distribute items.
func (s *DistributionService) CanDistribute(item *models.CapabilityItem, userID string, isPlatformAdmin bool) bool {
	return isPlatformAdmin
}

// DistributeItem distributes an item to the specified targets.
func (s *DistributionService) DistributeItem(ctx context.Context, item *models.CapabilityItem, distributorID string, req DistributeItemRequest) ([]DistributionResult, error) {
	results := make([]DistributionResult, 0, len(req.Targets))

	for _, target := range req.Targets {
		result, err := s.distributeToTarget(ctx, item, distributorID, target, req.PermissionMode, req.Message, req.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("distribute to target %s/%s failed: %w", target.ScopeType, target.TargetID, err)
		}
		results = append(results, *result)
	}

	return results, nil
}

func (s *DistributionService) distributeToTarget(ctx context.Context, item *models.CapabilityItem, distributorID string, target DistributionTarget, permissionMode, message string, expiresAt *time.Time) (*DistributionResult, error) {
	dist := &models.ItemDistribution{
		ID:             uuid.New().String(),
		ItemID:         item.ID,
		DistributorID:  distributorID,
		PermissionMode: permissionMode,
		Status:         "active",
		ScopeType:      target.ScopeType,
		TargetID:       target.TargetID,
		Message:        message,
		ExpiresAt:      expiresAt,
	}

	var recipientCount int
	var recipients []string

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(dist).Error; err != nil {
			return err
		}

		// Resolve recipients and create receipts
		resolved, err := s.resolveRecipients(tx, target)
		if err != nil {
			return err
		}
		recipients = resolved
		recipientCount = len(recipients)

		for _, userID := range recipients {
			receipt := models.ItemDistributionReceipt{
				ID:             uuid.New().String(),
				DistributionID: dist.ID,
				UserID:         userID,
				ReceiptStatus:  "unread",
			}
			// Use insert-ignore pattern to avoid duplicates
			if err := tx.Create(&receipt).Error; err != nil {
				// If duplicate, continue
				if !isUniqueConstraintError(err) {
					return err
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Auto-favorite each recipient AFTER the distribution has committed. This is
	// best-effort: a favorite hiccup must not roll back an otherwise-valid
	// distribution, and creating it post-commit avoids leaving an orphan favorite
	// if the transaction had rolled back. (In-tx it can't be best-effort: any error
	// inside the tx aborts the whole tx in Postgres.) The recipient is distributed
	// regardless; the favorite is the auto-install convenience.
	if s.behaviorSvc != nil {
		for _, userID := range recipients {
			// Distributed favorites default to "auto" (AI-auto-invokable).
			_, _, _ = s.behaviorSvc.FavoriteItem(ctx, item.ID, userID, "auto")
		}
	}

	// Notify recipients. Carry the distributor's message (附言) into the body so
	// recipients actually see the note written at distribution time, not just the
	// generic "someone shared a skill" line.
	if s.notificationSvc != nil {
		body := fmt.Sprintf("有人向你下发了技能 **%s**（权限：%s）", item.Name, permissionMode)
		if strings.TrimSpace(message) != "" {
			body += fmt.Sprintf("\n\n附言：%s", message)
		}
		for _, userID := range recipients {
			s.notificationSvc.TriggerMessage(userID, "item.distributed", sender.NotificationMessage{
				Title:     "技能下发",
				Body:      body,
				EventType: "item.distributed",
				Metadata: map[string]any{
					"itemId":         item.ID,
					"itemName":       item.Name,
					"permissionMode": permissionMode,
					"distributionId": dist.ID,
					"message":        message,
				},
			})
		}
	}

	return &DistributionResult{
		Distribution:   dist,
		RecipientCount: recipientCount,
	}, nil
}

// resolveRecipients resolves the list of user IDs for a given target.
func (s *DistributionService) resolveRecipients(tx *gorm.DB, target DistributionTarget) ([]string, error) {
	switch target.ScopeType {
	case "user":
		return []string{target.TargetID}, nil
	case "organization":
		var userIDs []string
		// Exclude users without a subject_id (mirrors notification resolveBroadcastRecipients).
		if err := tx.Model(&models.User{}).Where("organization = ? AND subject_id <> ''", target.TargetID).Pluck("subject_id", &userIDs).Error; err != nil {
			return nil, err
		}
		return userIDs, nil
	case "department", "role":
		// Reserved for future extension
		return []string{}, nil
	default:
		return nil, fmt.Errorf("unsupported scope type: %s", target.ScopeType)
	}
}

// DistributionListFilter holds filters for the global (platform admin) distribution list.
type DistributionListFilter struct {
	Status    string // active | paused | revoked | "" (all)
	ScopeType string // user | organization | "" (all)
	Search    string // optional: matches item name / distributor id / target id
	Page      int    // 1-based
	PageSize  int    // defaults to 20
}

// ListAllDistributions lists distributions across all distributors (platform admin view),
// with optional status/scope/search filters and pagination.
func (s *DistributionService) ListAllDistributions(ctx context.Context, f DistributionListFilter) ([]models.ItemDistribution, int64, error) {
	q := s.db.WithContext(ctx).Model(&models.ItemDistribution{})

	if f.Status != "" {
		q = q.Where("status = ?", f.Status)
	}
	if f.ScopeType != "" {
		q = q.Where("scope_type = ?", f.ScopeType)
	}
	if f.Search != "" {
		like := "%" + f.Search + "%"
		q = q.Where(
			"distributor_id LIKE ? OR target_id LIKE ? OR item_id IN (?)",
			like, like,
			s.db.Model(&models.CapabilityItem{}).Select("id").Where("name LIKE ?", like),
		)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	page := f.Page
	if page < 1 {
		page = 1
	}
	size := f.PageSize
	if size <= 0 {
		size = 20
	}

	var list []models.ItemDistribution
	if err := q.
		Preload("Item").
		Order("created_at DESC").
		Offset((page - 1) * size).
		Limit(size).
		Find(&list).Error; err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// ListReceipts lists all receipts for a given distribution (drives the detail drawer).
func (s *DistributionService) ListReceipts(ctx context.Context, distID string) ([]models.ItemDistributionReceipt, error) {
	var receipts []models.ItemDistributionReceipt
	if err := s.db.WithContext(ctx).
		Where("distribution_id = ?", distID).
		Order("created_at DESC").
		Find(&receipts).Error; err != nil {
		return nil, err
	}
	return receipts, nil
}

// ListItemDistributions lists all distributions for a given item.
func (s *DistributionService) ListItemDistributions(ctx context.Context, itemID string) ([]models.ItemDistribution, error) {
	var distributions []models.ItemDistribution
	if err := s.db.WithContext(ctx).Where("item_id = ?", itemID).Order("created_at DESC").Find(&distributions).Error; err != nil {
		return nil, err
	}
	return distributions, nil
}

// ListSentDistributions lists distributions sent by a user.
func (s *DistributionService) ListSentDistributions(ctx context.Context, distributorID string) ([]models.ItemDistribution, error) {
	var distributions []models.ItemDistribution
	if err := s.db.WithContext(ctx).Where("distributor_id = ?", distributorID).Preload("Item").Order("created_at DESC").Find(&distributions).Error; err != nil {
		return nil, err
	}
	return distributions, nil
}

// ListReceivedDistributions lists distributions received by a user (with item details).
func (s *DistributionService) ListReceivedDistributions(ctx context.Context, userID string) ([]models.ItemDistributionReceipt, error) {
	var receipts []models.ItemDistributionReceipt
	if err := s.db.WithContext(ctx).
		Joins("JOIN item_distributions ON item_distributions.id = item_distribution_receipts.distribution_id").
		Where("item_distribution_receipts.user_id = ? AND item_distribution_receipts.receipt_status != ? AND item_distributions.status = ?", userID, "dismissed", "active").
		Preload("Distribution.Item").
		Order("item_distribution_receipts.created_at DESC").
		Find(&receipts).Error; err != nil {
		return nil, err
	}
	return receipts, nil
}

// GetDistributionByID fetches a distribution by ID.
func (s *DistributionService) GetDistributionByID(ctx context.Context, id string) (*models.ItemDistribution, error) {
	var dist models.ItemDistribution
	if err := s.db.WithContext(ctx).First(&dist, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrDistributionNotFound
		}
		return nil, err
	}
	return &dist, nil
}

// UpdateDistribution updates a distribution's status, permission mode, or message.
func (s *DistributionService) UpdateDistribution(ctx context.Context, distID, operatorID string, isPlatformAdmin bool, status, permissionMode, message *string) (*models.ItemDistribution, error) {
	dist, err := s.GetDistributionByID(ctx, distID)
	if err != nil {
		return nil, err
	}

	if !s.canModifyDistribution(dist, operatorID, isPlatformAdmin) {
		return nil, ErrNotDistributor
	}

	updates := make(map[string]interface{})
	if status != nil {
		updates["status"] = *status
		if *status == "revoked" {
			now := time.Now()
			updates["revoked_at"] = &now
		}
	}
	if permissionMode != nil {
		updates["permission_mode"] = *permissionMode
	}
	if message != nil {
		updates["message"] = *message
	}

	if len(updates) == 0 {
		return dist, nil
	}

	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(dist).Updates(updates).Error; err != nil {
			return err
		}

		// If revoked or paused, remove favorites for recipients
		if status != nil && (*status == "revoked" || *status == "paused") {
			var receipts []models.ItemDistributionReceipt
			if err := tx.Where("distribution_id = ?", dist.ID).Find(&receipts).Error; err != nil {
				return err
			}
			for _, receipt := range receipts {
				if s.behaviorSvc != nil {
					// Same tx as the status update, so the readonly guard sees this
					// distribution as already revoked/paused and removes the favorite
					// instead of treating the skill as still-required. ErrSkillRequired
					// (another active readonly distribution still needs it) is an
					// expected, non-fatal outcome — keep the favorite and continue. Any
					// OTHER error is a real failure: propagate it so the whole tx
					// (including the revoked/paused status change) rolls back rather than
					// committing a status that's out of sync with the favorite.
					if _, _, err := s.behaviorSvc.UnfavoriteItemTx(tx, dist.ItemID, receipt.UserID); err != nil && !errors.Is(err, ErrSkillRequired) {
						return err
					}
				}
			}
			if *status == "revoked" {
				if err := tx.Model(&models.ItemDistributionReceipt{}).Where("distribution_id = ?", dist.ID).Update("receipt_status", "dismissed").Error; err != nil {
					return err
				}
			}
		}

		// If resumed to active, re-add favorites
		if status != nil && *status == "active" {
			var receipts []models.ItemDistributionReceipt
			if err := tx.Where("distribution_id = ? AND receipt_status != ?", dist.ID, "dismissed").Find(&receipts).Error; err != nil {
				return err
			}
			for _, receipt := range receipts {
				if s.behaviorSvc != nil {
					// Re-favorite within the same tx; a real failure must roll back the
					// resume so status and favorite stay consistent.
					if _, _, err := s.behaviorSvc.FavoriteItemTx(tx, dist.ItemID, receipt.UserID); err != nil {
						return err
					}
				}
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Notify recipients on revoke or pause
	if s.notificationSvc != nil && status != nil && (*status == "revoked" || *status == "paused") {
		var receipts []models.ItemDistributionReceipt
		if err := s.db.WithContext(ctx).Where("distribution_id = ?", dist.ID).Find(&receipts).Error; err == nil {
			var item models.CapabilityItem
			_ = s.db.WithContext(ctx).First(&item, "id = ?", dist.ItemID).Error
			for _, receipt := range receipts {
				body := fmt.Sprintf("技能 **%s** 的下发已被%s", item.Name, map[string]string{"revoked": "收回", "paused": "暂停"}[*status])
				s.notificationSvc.TriggerMessage(receipt.UserID, "item."+*status, sender.NotificationMessage{
					Title:     "技能下发更新",
					Body:      body,
					EventType: "item." + *status,
					Metadata: map[string]any{
						"itemId":         dist.ItemID,
						"itemName":       item.Name,
						"distributionId": dist.ID,
						"status":         *status,
					},
				})
			}
		}
	}

	return s.GetDistributionByID(ctx, distID)
}

// RevokeDistribution revokes a distribution (soft delete).
func (s *DistributionService) RevokeDistribution(ctx context.Context, distID, operatorID string, isPlatformAdmin bool) error {
	_, err := s.UpdateDistribution(ctx, distID, operatorID, isPlatformAdmin, strPtr("revoked"), nil, nil)
	return err
}

// DismissReceipt allows a recipient to dismiss a distribution from their view.
func (s *DistributionService) DismissReceipt(ctx context.Context, distID, userID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&models.ItemDistributionReceipt{}).
			Where("distribution_id = ? AND user_id = ?", distID, userID).
			Update("receipt_status", "dismissed")
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errors.New("receipt not found")
		}

		// Remove favorite
		var dist models.ItemDistribution
		if err := tx.Where("id = ?", distID).First(&dist).Error; err == nil {
			if s.behaviorSvc != nil {
				_, _, _ = s.behaviorSvc.UnfavoriteItem(ctx, dist.ItemID, userID)
			}
		}
		return nil
	})
}

// MarkReceiptRead marks a receipt as read.
func (s *DistributionService) MarkReceiptRead(ctx context.Context, distID, userID string) error {
	result := s.db.WithContext(ctx).Model(&models.ItemDistributionReceipt{}).
		Where("distribution_id = ? AND user_id = ?", distID, userID).
		Update("receipt_status", "read")
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("receipt not found")
	}
	return nil
}

// GetReceiptByDistributionAndUser gets a receipt for a specific distribution and user.
func (s *DistributionService) GetReceiptByDistributionAndUser(ctx context.Context, distID, userID string) (*models.ItemDistributionReceipt, error) {
	var receipt models.ItemDistributionReceipt
	if err := s.db.WithContext(ctx).Where("distribution_id = ? AND user_id = ?", distID, userID).First(&receipt).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("receipt not found")
		}
		return nil, err
	}
	return &receipt, nil
}

// GetEffectivePermission returns the effective permission mode (readonly |
// dismissible) for a user on an item, derived from the most recent active,
// non-dismissed distribution receipt. The bool reports whether such a
// distribution exists.
func (s *DistributionService) GetEffectivePermission(ctx context.Context, itemID, userID string) (string, bool) {
	var modes []string
	err := s.db.WithContext(ctx).
		Model(&models.ItemDistributionReceipt{}).
		Joins("JOIN item_distributions ON item_distributions.id = item_distribution_receipts.distribution_id").
		Where("item_distribution_receipts.user_id = ? AND item_distributions.item_id = ? AND item_distributions.status = ? AND item_distribution_receipts.receipt_status != ?",
			userID, itemID, "active", "dismissed").
		Order("item_distributions.created_at DESC").
		Limit(1).
		Pluck("item_distributions.permission_mode", &modes).Error

	if err != nil || len(modes) == 0 {
		return "", false
	}
	return modes[0], true
}

// canModifyDistribution checks if an operator can modify a distribution.
func (s *DistributionService) canModifyDistribution(dist *models.ItemDistribution, operatorID string, isPlatformAdmin bool) bool {
	if isPlatformAdmin {
		return true
	}
	return dist.DistributorID == operatorID
}

// Helper functions

func strPtr(s string) *string {
	return &s
}


