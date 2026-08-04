package models

import (
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

func TestCapabilityItem_GitIdentityIndexIsOwnedByGoose(t *testing.T) {
	// AutoMigrate derives indexes from this parsed schema. Both Git identity
	// indexes are partial and therefore must stay owned by the PostgreSQL migration.
	capabilitySchema, err := schema.Parse(&CapabilityItem{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse CapabilityItem schema: %v", err)
	}
	parsedIndexes := make(map[string]struct{})
	for _, index := range capabilitySchema.ParseIndexes() {
		parsedIndexes[index.Name] = struct{}{}
	}
	for _, migrationOwnedIndex := range []string{
		"idx_capability_items_git_repo",
		"uq_capability_items_git_manifest",
	} {
		if _, exists := parsedIndexes[migrationOwnedIndex]; exists {
			t.Fatalf("AutoMigrate must not create %s; Goose owns the partial Git identity indexes", migrationOwnedIndex)
		}
	}
}
