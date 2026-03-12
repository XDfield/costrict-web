package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/worker"
	"gorm.io/gorm"
)

func main() {
	cfg := config.Load()

	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	err = db.AutoMigrate(
		&models.SyncLog{},
		&models.SyncJob{},
		&models.CapabilityRegistry{},
		&models.CapabilityItem{},
		&models.CapabilityVersion{},
		&models.CapabilityAsset{},
	)
	if err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
	}

	if err := runPostMigrations(db); err != nil {
		log.Fatalf("Failed to run post-migrations: %v", err)
	}

	tmpDir := os.Getenv("SYNC_TMP_DIR")
	if tmpDir == "" {
		tmpDir = os.TempDir() + "/costrict-sync"
	}

	syncSvc := &services.SyncService{
		DB:     db,
		Git:    &services.GitService{TempBaseDir: tmpDir},
		Parser: &services.ParserService{},
	}

	concurrency, _ := strconv.Atoi(os.Getenv("WORKER_CONCURRENCY"))
	if concurrency <= 0 {
		concurrency = 3
	}

	pollInterval := 5 * time.Second
	if v := os.Getenv("WORKER_POLL_INTERVAL_SECONDS"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
			pollInterval = time.Duration(secs) * time.Second
		}
	}

	pool := &worker.WorkerPool{
		DB:           db,
		SyncService:  syncSvc,
		Concurrency:  concurrency,
		PollInterval: pollInterval,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool.Start()
	log.Printf("Worker pool started with %d workers, polling every %s", concurrency, pollInterval)

	<-ctx.Done()
	log.Println("Shutting down worker pool...")
	pool.Stop()
	log.Println("Worker pool stopped")
}

func runPostMigrations(db *gorm.DB) error {
	fks := []struct {
		table      string
		constraint string
		stmt       string
	}{
		{
			table:      "sync_logs",
			constraint: "fk_sync_logs_registry",
			stmt:       `ALTER TABLE sync_logs ADD CONSTRAINT fk_sync_logs_registry FOREIGN KEY (registry_id) REFERENCES capability_registries(id)`,
		},
		{
			table:      "sync_jobs",
			constraint: "fk_sync_jobs_registry",
			stmt:       `ALTER TABLE sync_jobs ADD CONSTRAINT fk_sync_jobs_registry FOREIGN KEY (registry_id) REFERENCES capability_registries(id)`,
		},
		{
			table:      "capability_registries",
			constraint: "fk_capability_registries_last_sync_log",
			stmt:       `ALTER TABLE capability_registries ADD CONSTRAINT fk_capability_registries_last_sync_log FOREIGN KEY (last_sync_log_id) REFERENCES sync_logs(id) ON DELETE SET NULL`,
		},
	}

	for _, fk := range fks {
		var exists int
		db.Raw(`SELECT 1 FROM information_schema.table_constraints WHERE table_name=? AND constraint_name=?`, fk.table, fk.constraint).Scan(&exists)
		if exists == 1 {
			continue
		}
		if err := db.Exec(fk.stmt).Error; err != nil {
			return fmt.Errorf("post-migration failed (%s): %w", fk.stmt, err)
		}
	}
	return nil
}
