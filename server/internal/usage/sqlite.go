package usage

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func OpenSQLite(path string) (*gorm.DB, error) {
	if path == "" {
		path = "./data/usage/usage.db"
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create usage sqlite dir: %w", err)
	}

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open usage sqlite: %w", err)
	}

	if err := db.AutoMigrate(&models.SessionUsageReport{}); err != nil {
		return nil, fmt.Errorf("migrate usage sqlite: %w", err)
	}

	return db, nil
}
