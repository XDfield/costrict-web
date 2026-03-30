package services

import (
	"context"
	"log"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// IndexerService handles indexing capability items for semantic search
type IndexerService struct {
	db              *gorm.DB
	embeddingSvc    *EmbeddingService
	batchSize       int
}

// NewIndexerService creates a new indexer service
func NewIndexerService(db *gorm.DB, embeddingSvc *EmbeddingService) *IndexerService {
	return &IndexerService{
		db:           db,
		embeddingSvc: embeddingSvc,
		batchSize:    50,
	}
}

// IndexAll indexes all capability items that don't have embeddings
func (s *IndexerService) IndexAll(ctx context.Context) error {
	log.Println("Starting full index of capability items...")

	var items []models.CapabilityItem

	// Find items without embeddings
	result := s.db.Where("embedding IS NULL").
		Where("status = ?", "active").
		Find(&items)

	if result.Error != nil {
		return result.Error
	}

	log.Printf("Found %d items to index", len(items))

	return s.IndexItems(ctx, items)
}

// IndexItems indexes a batch of items
func (s *IndexerService) IndexItems(ctx context.Context, items []models.CapabilityItem) error {
	if len(items) == 0 {
		return nil
	}

	for i := 0; i < len(items); i += s.batchSize {
		end := i + s.batchSize
		if end > len(items) {
			end = len(items)
		}

		batch := items[i:end]
		if err := s.indexBatch(ctx, batch); err != nil {
			log.Printf("Error indexing batch %d-%d: %v", i, end, err)
			// Continue with next batch
		}
	}

	return nil
}

// indexBatch indexes a batch of items
func (s *IndexerService) indexBatch(ctx context.Context, items []models.CapabilityItem) error {
	// Prepare texts for embedding
	texts := make([]string, len(items))
	for i, item := range items {
		texts[i] = s.embeddingSvc.buildItemText(item.Name, item.Description, item.Content, item.ItemType)
	}

	// Get embeddings
	embeddings, err := s.embeddingSvc.GetEmbedding(ctx, texts)
	if err != nil {
		return err
	}

	// Update items with embeddings
	now := time.Now()
	for i, item := range items {
		if i < len(embeddings) {
			vectorStr := FormatVectorForDB(embeddings[i])
			result := s.db.Model(&models.CapabilityItem{}).
				Where("id = ?", item.ID).
				Updates(map[string]interface{}{
					"embedding":           vectorStr,
					"embedding_updated_at": now,
				})

			if result.Error != nil {
				log.Printf("Error updating embedding for item %s: %v", item.ID, result.Error)
			}
		}
	}

	return nil
}

// IndexItem indexes a single item
func (s *IndexerService) IndexItem(ctx context.Context, item *models.CapabilityItem) error {
	embedding, err := s.embeddingSvc.EmbedItem(ctx, item.Name, item.Description, item.Content, item.ItemType)
	if err != nil {
		return err
	}

	vectorStr := FormatVectorForDB(embedding)
	now := time.Now()

	return s.db.Model(item).Updates(map[string]interface{}{
		"embedding":            vectorStr,
		"embedding_updated_at": now,
	}).Error
}

// ReindexItem re-indexes a specific item by ID
func (s *IndexerService) ReindexItem(ctx context.Context, itemID string) error {
	var item models.CapabilityItem
	result := database.GetDB().First(&item, "id = ?", itemID)
	if result.Error != nil {
		return result.Error
	}

	return s.IndexItem(ctx, &item)
}

// GetIndexStats returns statistics about the index
func (s *IndexerService) GetIndexStats(ctx context.Context) (*IndexStats, error) {
	stats := &IndexStats{}

	// Count total items
	result := s.db.Model(&models.CapabilityItem{}).Count(&stats.TotalItems)
	if result.Error != nil {
		return nil, result.Error
	}

	// Count indexed items
	result = s.db.Model(&models.CapabilityItem{}).
		Where("embedding IS NOT NULL").
		Count(&stats.IndexedItems)
	if result.Error != nil {
		return nil, result.Error
	}

	// Count items pending indexing
	stats.PendingItems = stats.TotalItems - stats.IndexedItems

	return stats, nil
}

// IndexStats contains statistics about the vector index
type IndexStats struct {
	TotalItems    int64 `json:"totalItems"`
	IndexedItems  int64 `json:"indexedItems"`
	PendingItems  int64 `json:"pendingItems"`
}

// StartBackgroundIndexing starts a background goroutine to index pending items
func (s *IndexerService) StartBackgroundIndexing(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.IndexAll(ctx); err != nil {
					log.Printf("Background indexing error: %v", err)
				}
			}
		}
	}()
}
