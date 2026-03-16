package services

import (
	"context"
	"fmt"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/llm"
	"github.com/costrict/costrict-web/server/internal/models"
)

// GenerateService handles skill analysis and improvement using LLM
type GenerateService struct {
	llmClient  *llm.Client
	indexerSvc *IndexerService
}

// NewGenerateService creates a new generate service
func NewGenerateService(llmClient *llm.Client, indexerSvc *IndexerService) *GenerateService {
	return &GenerateService{
		llmClient:  llmClient,
		indexerSvc: indexerSvc,
	}
}

// AnalyzeSkill analyzes an existing skill and suggests improvements
func (s *GenerateService) AnalyzeSkill(ctx context.Context, itemID string) (*llm.SkillAnalysis, error) {
	db := database.GetDB()

	var item models.CapabilityItem
	result := db.First(&item, "id = ?", itemID)
	if result.Error != nil {
		return nil, fmt.Errorf("item not found: %w", result.Error)
	}

	// Build prompts
	systemPrompt, userPrompt := llm.BuildSkillImprovePrompt(
		item.Name,
		item.ItemType,
		item.Description,
		item.Content,
		string(item.Metadata),
	)

	// Call LLM
	response, err := s.llmClient.ChatSimple(systemPrompt, userPrompt)
	if err != nil {
		return nil, fmt.Errorf("failed to analyze skill: %w", err)
	}

	// Parse response
	analysis, err := llm.ParseSkillAnalysis(response)
	if err != nil {
		return nil, fmt.Errorf("failed to parse analysis: %w", err)
	}

	return analysis, nil
}

// ImproveSkill applies suggested improvements to a skill
func (s *GenerateService) ImproveSkill(ctx context.Context, itemID string, improvements []llm.SkillImprovement, updatedBy string) (*models.CapabilityItem, error) {
	db := database.GetDB()

	var item models.CapabilityItem
	result := db.First(&item, "id = ?", itemID)
	if result.Error != nil {
		return nil, fmt.Errorf("item not found: %w", result.Error)
	}

	// Apply improvements
	for _, imp := range improvements {
		switch imp.Field {
		case "description":
			item.Description = imp.Suggested
		case "content":
			item.Content = imp.Suggested
		}
	}

	item.UpdatedBy = updatedBy

	// Save changes
	if result := db.Save(&item); result.Error != nil {
		return nil, result.Error
	}

	// Re-index the item
	go func() {
		_ = s.indexerSvc.IndexItem(context.Background(), &item)
	}()

	return &item, nil
}

