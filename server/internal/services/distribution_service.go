package services

import (
	"context"
	"errors"
	"fmt"
	"log"
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
	PermissionMode string               `json:"permissionMode" binding:"required,oneof=readonly forkable editable"`
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
	ErrCannotDistribute    = errors.New("you do not have permission to distribute this item")
	ErrAlreadyForked       = errors.New("item already forked")
	ErrForkNotAllowed      = errors.New("fork not allowed for this distribution")
)

// CanDistribute checks if a user can distribute an item.
func (s *DistributionService) CanDistribute(item *models.CapabilityItem, userID string, isPlatformAdmin bool) bool {
	if isPlatformAdmin {
		return true
	}
	if item.CreatedBy == userID {
		return true
	}
	// Check repo admin role
	if item.Registry != nil && item.Registry.RepoID != "" {
		var member models.RepoMember
		if err := s.db.Where("repo_id = ? AND user_id = ?", item.Registry.RepoID, userID).First(&member).Error; err == nil {
			if member.Role == "owner" || member.Role == "admin" {
				return true
			}
		}
	}
	return false
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

			// Auto-create favorite for the recipient
			if s.behaviorSvc != nil {
				_, _, _ = s.behaviorSvc.FavoriteItem(ctx, item.ID, userID)
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	// Notify recipients
	if s.notificationSvc != nil {
		for _, userID := range recipients {
			s.notificationSvc.TriggerMessage(userID, "item.distributed", sender.NotificationMessage{
				Title:     "技能下发",
				Body:      fmt.Sprintf("有人向你下发了技能 **%s**（权限：%s）", item.Name, permissionMode),
				EventType: "item.distributed",
				Metadata: map[string]any{
					"itemId":         item.ID,
					"itemName":       item.Name,
					"permissionMode": permissionMode,
					"distributionId": dist.ID,
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
		if err := tx.Model(&models.User{}).Where("organization = ?", target.TargetID).Pluck("subject_id", &userIDs).Error; err != nil {
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
	if err := s.db.WithContext(ctx).Where("distributor_id = ?", distributorID).Order("created_at DESC").Find(&distributions).Error; err != nil {
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
					_, _, _ = s.behaviorSvc.UnfavoriteItem(ctx, dist.ItemID, receipt.UserID)
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
					_, _, _ = s.behaviorSvc.FavoriteItem(ctx, dist.ItemID, receipt.UserID)
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

// ForkItem creates a personal copy of an item for the recipient.
func (s *DistributionService) ForkItem(ctx context.Context, distID, userID string) (*models.CapabilityItem, error) {
	var receipt models.ItemDistributionReceipt
	if err := s.db.WithContext(ctx).
		Joins("JOIN item_distributions ON item_distributions.id = item_distribution_receipts.distribution_id").
		Where("item_distribution_receipts.distribution_id = ? AND item_distribution_receipts.user_id = ? AND item_distributions.permission_mode = ? AND item_distributions.status = ?",
			distID, userID, "forkable", "active").
		Preload("Distribution.Item").
		First(&receipt).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrForkNotAllowed
		}
		return nil, err
	}

	if receipt.ForkedItemID != nil && *receipt.ForkedItemID != "" {
		return nil, ErrAlreadyForked
	}

	// Find or create personal registry for the user
	personalRegistryID, err := s.getOrCreatePersonalRegistry(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("failed to get personal registry: %w", err)
	}

	origItem := receipt.Distribution.Item
	if origItem == nil {
		// Load item if not preloaded
		var item models.CapabilityItem
		if err := s.db.WithContext(ctx).First(&item, "id = ?", receipt.Distribution.ItemID).Error; err != nil {
			return nil, err
		}
		origItem = &item
	}

	// Copy the item
	forkedItem := &models.CapabilityItem{
		ID:              uuid.New().String(),
		RegistryID:      personalRegistryID,
		RepoID:          "", // Will be set by registryRepoID later
		Slug:            origItem.Slug + "-fork-" + userID[:8],
		ItemType:        origItem.ItemType,
		Name:            origItem.Name + " (Fork)",
		Description:     origItem.Description,
		Category:        origItem.Category,
		Version:         origItem.Version,
		Content:         origItem.Content,
		ContentMD5:      origItem.ContentMD5,
		CurrentRevision: 1,
		Metadata:        origItem.Metadata,
		SourcePath:      origItem.SourcePath,
		SourceSHA:       origItem.SourceSHA,
		SourceType:      "fork",
		Source:          receipt.DistributionID,
		Status:          "active",
		CreatedBy:       userID,
	}

	// Use transaction
	var resultItem *models.CapabilityItem
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Embedding").Create(forkedItem).Error; err != nil {
			return err
		}

		// Create initial version
		version := models.CapabilityVersion{
			ID:          uuid.New().String(),
			ItemID:      forkedItem.ID,
			Revision:    1,
			Name:        forkedItem.Name,
			Description: forkedItem.Description,
			Category:    forkedItem.Category,
			Version:     forkedItem.Version,
			Content:     forkedItem.Content,
			ContentMD5:  forkedItem.ContentMD5,
			Metadata:    forkedItem.Metadata,
			SourcePath:  forkedItem.SourcePath,
			CommitMsg:   "Forked from distribution " + receipt.DistributionID,
			CreatedBy:   userID,
		}
		if err := tx.Create(&version).Error; err != nil {
			return err
		}

		// Update receipt with forked item ID
		if err := tx.Model(&receipt).Update("forked_item_id", forkedItem.ID).Error; err != nil {
			return err
		}

		resultItem = forkedItem
		return nil
	})

	if err != nil {
		return nil, err
	}

	return resultItem, nil
}

// getOrCreatePersonalRegistry finds or creates a personal registry for a user.
func (s *DistributionService) getOrCreatePersonalRegistry(ctx context.Context, userID string) (string, error) {
	var registry models.CapabilityRegistry
	personalName := "user-" + userID + "-personal"

	if err := s.db.WithContext(ctx).Where("name = ?", personalName).First(&registry).Error; err == nil {
		return registry.ID, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return "", err
	}

	// Need to create a personal repo first
	var repo models.Repository
	repoName := "personal-" + userID
	if err := s.db.WithContext(ctx).Where("name = ?", repoName).First(&repo).Error; err == nil {
		// Repo exists, use it
	} else if errors.Is(err, gorm.ErrRecordNotFound) {
		repo = models.Repository{
			ID:          uuid.New().String(),
			Name:        repoName,
			DisplayName: "Personal",
			Visibility:  "private",
			RepoType:    "normal",
			OwnerID:     userID,
		}
		if err := s.db.WithContext(ctx).Create(&repo).Error; err != nil {
			return "", err
		}

		// Add owner as member
		member := models.RepoMember{
			ID:     uuid.New().String(),
			RepoID: repo.ID,
			UserID: userID,
			Role:   "owner",
		}
		if err := s.db.WithContext(ctx).Create(&member).Error; err != nil {
			return "", err
		}
	} else {
		return "", err
	}

	// Create registry
	registry = models.CapabilityRegistry{
		ID:         uuid.New().String(),
		Name:       personalName,
		RepoID:     repo.ID,
		OwnerID:    userID,
		SourceType: "internal",
	}
	if err := s.db.WithContext(ctx).Create(&registry).Error; err != nil {
		return "", err
	}

	return registry.ID, nil
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

// GetEffectivePermission returns the effective permission mode for a user on an item.
func (s *DistributionService) GetEffectivePermission(ctx context.Context, itemID, userID string) (string, bool) {
	var receipt models.ItemDistributionReceipt
	err := s.db.WithContext(ctx).
		Joins("JOIN item_distributions ON item_distributions.id = item_distribution_receipts.distribution_id").
		Where("item_distribution_receipts.user_id = ? AND item_distributions.item_id = ? AND item_distributions.status = ? AND item_distribution_receipts.receipt_status != ?",
			userID, itemID, "active", "dismissed").
		Select("item_distributions.permission_mode").
		Scan(&receipt).Error

	if err != nil || receipt.ID == "" {
		return "", false
	}
	return receipt.ReceiptStatus, true // Note: this needs a join to get permission_mode, simplified here
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


// GetEffectivePermissionForItem returns whether a user has editable access to an item via distribution.
func (s *DistributionService) HasEditableDistribution(ctx context.Context, itemID, userID string) bool {
	var count int64
	err := s.db.WithContext(ctx).
		Model(&models.ItemDistributionReceipt{}).
		Joins("JOIN item_distributions ON item_distributions.id = item_distribution_receipts.distribution_id").
		Where("item_distribution_receipts.user_id = ? AND item_distributions.item_id = ? AND item_distributions.status = ? AND item_distributions.permission_mode = ? AND item_distribution_receipts.receipt_status != ?",
			userID, itemID, "active", "editable", "dismissed").
		Count(&count).Error

	if err != nil {
		log.Printf("[distribution] HasEditableDistribution query error: %v", err)
		return false
	}
	return count > 0
}
