package handlers

import (
	"log"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
)

const PublicRegistryID = "00000000-0000-0000-0000-000000000001"

func EnsurePublicRegistry() {
	db := database.GetDB()
	var registry models.CapabilityRegistry
	result := db.First(&registry, "id = ?", PublicRegistryID)
	if result.Error == nil {
		return
	}

	registry = models.CapabilityRegistry{
		ID:          PublicRegistryID,
		Name:        "public",
		Description: "Default public registry — anyone can browse and contribute",
		SourceType:  "internal",
		RepoID:      "public",
		OwnerID:     "system",
	}
	if err := db.Create(&registry).Error; err != nil {
		log.Printf("Warning: failed to create public registry: %v", err)
	} else {
		log.Println("Public registry initialized")
	}
}
