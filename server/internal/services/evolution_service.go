package services

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/costrict/costrict-web/server/internal/llm"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// EvolutionService handles experience evolution and pattern analysis
type EvolutionService struct {
	db         *gorm.DB
	llmClient  *llm.Client
	behaviorSvc *BehaviorService
}

// NewEvolutionService creates a new evolution service
func NewEvolutionService(db *gorm.DB, llmClient *llm.Client, behaviorSvc *BehaviorService) *EvolutionService {
	return &EvolutionService{
		db:         db,
		llmClient:  llmClient,
		behaviorSvc: behaviorSvc,
	}
}

// AnalyzePatterns analyzes behavior patterns for an item
func (s *EvolutionService) AnalyzePatterns(ctx context.Context, itemID string) (*llm.ExperienceAnalysisResult, error) {
	// Get item details
	var item models.CapabilityItem
	result := s.db.First(&item, "id = ?", itemID)
	if result.Error != nil {
		return nil, result.Error
	}

	// Get recent behavior logs
	logs, err := s.behaviorSvc.GetBehaviorsByTimeRange(ctx, time.Now().AddDate(0, -1, 0), time.Now(), itemID)
	if err != nil {
		return nil, err
	}

	if len(logs) == 0 {
		return nil, nil
	}

	// Convert logs to JSON for LLM
	logsJSON, _ := json.MarshalIndent(logs, "", "  ")

	// Build prompts
	systemPrompt, userPrompt := llm.BuildExperienceAnalysisPrompt(
		item.Name,
		item.ItemType,
		item.Description,
		string(logsJSON),
	)

	// Call LLM
	response, err := s.llmClient.ChatSimple(systemPrompt, userPrompt)
	if err != nil {
		return nil, err
	}

	// Parse result
	return llm.ParseExperienceAnalysis(response)
}

// CreateCandidate creates a new experience candidate
func (s *EvolutionService) CreateCandidate(ctx context.Context, itemID string, candidate llm.CandidateExperience, sourceType models.SourceType, sourceLogID string) (*models.ExperienceCandidate, error) {
	// Determine experience type
	var expType models.ExperienceType
	switch candidate.Type {
	case "best_practice":
		expType = models.ExperiencePractice
	case "behavior_rule":
		expType = models.ExperienceRule
	case "feature_request":
		expType = models.ExperienceFeature
	case "error":
		expType = models.ExperienceError
	default:
		expType = models.ExperienceLearning
	}

	// Determine promotion type
	var promoType models.PromotionType
	switch candidate.Type {
	case "best_practice":
		promoType = models.PromotionBestPractice
	case "behavior_rule":
		promoType = models.PromotionBehaviorRule
	case "new_workflow":
		promoType = models.PromotionNewWorkflow
	case "new_skill":
		promoType = models.PromotionNewSkill
	}

	candidateRecord := &models.ExperienceCandidate{
		ID:            uuid.New().String(),
		ItemID:        itemID,
		Type:          expType,
		Title:         candidate.Title,
		Description:   candidate.Description,
		Context:       candidate.Context,
		SourceType:    sourceType,
		SourceLogID:   sourceLogID,
		Frequency:     candidate.Frequency,
		ImpactScore:   candidate.ImpactScore,
		Status:        models.StatusPending,
		PromotionType: promoType,
	}

	if result := s.db.Create(candidateRecord); result.Error != nil {
		return nil, result.Error
	}

	return candidateRecord, nil
}

// GetPendingCandidates returns pending experience candidates
func (s *EvolutionService) GetPendingCandidates(ctx context.Context, limit, offset int) ([]models.ExperienceCandidate, int64, error) {
	if limit <= 0 {
		limit = 20
	}

	var total int64
	s.db.Model(&models.ExperienceCandidate{}).
		Where("status = ?", models.StatusPending).
		Count(&total)

	var candidates []models.ExperienceCandidate
	result := s.db.Where("status = ?", models.StatusPending).
		Preload("Item").
		Order("impact_score DESC, frequency DESC").
		Limit(limit).
		Offset(offset).
		Find(&candidates)

	if result.Error != nil {
		return nil, 0, result.Error
	}

	return candidates, total, nil
}

// ApproveCandidate approves an experience candidate and promotes it
func (s *EvolutionService) ApproveCandidate(ctx context.Context, candidateID, approvedBy string) (*models.ExperiencePromotion, error) {
	// Get candidate
	var candidate models.ExperienceCandidate
	result := s.db.First(&candidate, "id = ?", candidateID)
	if result.Error != nil {
		return nil, result.Error
	}

	if candidate.Status != models.StatusPending {
		return nil, gorm.ErrRecordNotFound
	}

	// Get item and its current metadata
	var item models.CapabilityItem
	result = s.db.First(&item, "id = ?", candidate.ItemID)
	if result.Error != nil {
		return nil, result.Error
	}

	metadataBefore := item.Metadata

	// Promote the experience to item metadata
	if err := s.promoteToItem(&item, &candidate); err != nil {
		return nil, err
	}

	// Update candidate status
	now := time.Now()
	candidate.Status = models.StatusPromoted
	candidate.PromotedAt = &now
	candidate.PromotedBy = approvedBy
	s.db.Save(&candidate)

	// Create promotion record
	promotion := &models.ExperiencePromotion{
		ID:             uuid.New().String(),
		CandidateID:    candidateID,
		ItemID:         candidate.ItemID,
		PromotionType:  candidate.PromotionType,
		PromotedBy:     approvedBy,
		MetadataBefore: metadataBefore,
		MetadataAfter:  item.Metadata,
	}

	if result := s.db.Create(promotion); result.Error != nil {
		return nil, result.Error
	}

	return promotion, nil
}

// promoteToItem promotes an experience to item metadata
func (s *EvolutionService) promoteToItem(item *models.CapabilityItem, candidate *models.ExperienceCandidate) error {
	// Parse existing metadata
	var metadata map[string]interface{}
	if item.Metadata != nil {
		json.Unmarshal(item.Metadata, &metadata)
	}
	if metadata == nil {
		metadata = make(map[string]interface{})
	}

	// Add experience based on type
	switch candidate.PromotionType {
	case models.PromotionBestPractice:
		practices, _ := metadata["best_practices"].([]interface{})
		practice := models.BestPractice{
			Practice:    candidate.Description,
			Score:       candidate.ImpactScore,
			PromotedAt:  time.Now(),
			SourceCount: candidate.Frequency,
		}
		metadata["best_practices"] = append(practices, practice)

	case models.PromotionBehaviorRule:
		rules, _ := metadata["behavior_rules"].([]interface{})
		rule := models.BehaviorRule{
			Rule:    candidate.Title,
			Trigger: candidate.Context,
			Action:  candidate.Resolution,
		}
		metadata["behavior_rules"] = append(rules, rule)
	}

	// Update metadata
	newMetadata, _ := json.Marshal(metadata)
	item.Metadata = datatypes.JSON(newMetadata)

	return s.db.Model(item).Update("metadata", item.Metadata).Error
}

// RejectCandidate rejects an experience candidate
func (s *EvolutionService) RejectCandidate(ctx context.Context, candidateID, rejectedBy, reason string) error {
	result := s.db.Model(&models.ExperienceCandidate{}).
		Where("id = ? AND status = ?", candidateID, models.StatusPending).
		Updates(map[string]interface{}{
			"status":     models.StatusRejected,
			"promoted_by": rejectedBy,
			"promoted_at": time.Now(),
		})

	return result.Error
}

// RunAutomaticAnalysis runs automatic pattern analysis for all items
func (s *EvolutionService) RunAutomaticAnalysis(ctx context.Context) error {
	// Get items with recent activity
	thirtyDaysAgo := time.Now().AddDate(0, 0, -30)

	var itemIDs []string
	s.db.Model(&models.BehaviorLog{}).
		Select("DISTINCT item_id").
		Where("created_at > ?", thirtyDaysAgo).
		Pluck("item_id", &itemIDs)

	log.Printf("Running automatic analysis for %d items", len(itemIDs))

	for _, itemID := range itemIDs {
		result, err := s.AnalyzePatterns(ctx, itemID)
		if err != nil {
			log.Printf("Error analyzing item %s: %v", itemID, err)
			continue
		}

		if result == nil {
			continue
		}

		// Create candidates from analysis
		for _, candidate := range result.CandidateExperiences {
			if candidate.ImpactScore >= 0.5 { // Only create candidates with significant impact
				_, err := s.CreateCandidate(ctx, itemID, candidate, models.SourceAutoDetect, "")
				if err != nil {
					log.Printf("Error creating candidate for item %s: %v", itemID, err)
				}
			}
		}
	}

	return nil
}

// StartBackgroundAnalysis starts background pattern analysis
func (s *EvolutionService) StartBackgroundAnalysis(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.RunAutomaticAnalysis(ctx); err != nil {
					log.Printf("Background analysis error: %v", err)
				}
			}
		}
	}()
}

// GetCandidateByID returns a candidate by ID
func (s *EvolutionService) GetCandidateByID(ctx context.Context, candidateID string) (*models.ExperienceCandidate, error) {
	var candidate models.ExperienceCandidate
	result := s.db.Preload("Item").First(&candidate, "id = ?", candidateID)
	if result.Error != nil {
		return nil, result.Error
	}
	return &candidate, nil
}

// GetPromotionHistory returns promotion history for an item
func (s *EvolutionService) GetPromotionHistory(ctx context.Context, itemID string, limit int) ([]models.ExperiencePromotion, error) {
	if limit <= 0 {
		limit = 20
	}

	var promotions []models.ExperiencePromotion
	result := s.db.Where("item_id = ?", itemID).
		Preload("Candidate").
		Order("created_at DESC").
		Limit(limit).
		Find(&promotions)

	if result.Error != nil {
		return nil, result.Error
	}

	return promotions, nil
}
