package database

import (
	"fmt"
	"log"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var DB *gorm.DB

func Initialize(databaseURL string) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	log.Println("Database connected successfully")

	// Enable pgvector extension
	if err := enablePgVector(db); err != nil {
		log.Printf("Warning: Failed to enable pgvector extension: %v (continuing without vector support)", err)
	}

	DB = db
	return db, nil
}

// enablePgVector enables the pgvector extension in PostgreSQL
func enablePgVector(db *gorm.DB) error {
	// Create extension if not exists
	result := db.Exec("CREATE EXTENSION IF NOT EXISTS vector")
	if result.Error != nil {
		return fmt.Errorf("failed to create vector extension: %w", result.Error)
	}
	log.Println("pgvector extension enabled successfully")
	return nil
}

func GetDB() *gorm.DB {
	return DB
}
