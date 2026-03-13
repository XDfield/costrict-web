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
	"github.com/costrict/costrict-web/server/internal/llm"
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

	if err := runPreMigrations(db); err != nil {
		log.Fatalf("Failed to run pre-migrations: %v", err)
	}

	err = db.AutoMigrate(
		&models.SyncLog{},
		&models.SyncJob{},
		&models.CapabilityRegistry{},
		&models.CapabilityItem{},
		&models.CapabilityVersion{},
		&models.CapabilityAsset{},
		&models.SecurityScan{},
		&models.ScanJob{},
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

	scanEnabled := os.Getenv("SCAN_ENABLED")
	var scanPool *worker.ScanWorkerPool
	if scanEnabled != "false" {
		llmCfg := cfg.LLM
		if model := os.Getenv("SCAN_LLM_MODEL"); model != "" {
			llmCfg.Model = model
		}
		if timeoutStr := os.Getenv("SCAN_LLM_TIMEOUT_SECONDS"); timeoutStr != "" {
			if secs, err := strconv.Atoi(timeoutStr); err == nil && secs > 0 {
				llmCfg.MaxTokens = secs
			}
		}

		scanLLMClient := llm.NewClient(&llmCfg)
		scanSvc := &services.ScanService{
			DB:        db,
			LLMClient: scanLLMClient,
			ModelName: llmCfg.Model,
		}

		scanConcurrency, _ := strconv.Atoi(os.Getenv("SCAN_WORKER_CONCURRENCY"))
		if scanConcurrency <= 0 {
			scanConcurrency = 2
		}

		scanPool = &worker.ScanWorkerPool{
			DB:           db,
			ScanService:  scanSvc,
			Concurrency:  scanConcurrency,
			PollInterval: 3 * time.Second,
		}
		scanPool.Start()
		log.Printf("Scan worker pool started with %d workers", scanConcurrency)
	} else {
		log.Println("Scan worker pool disabled (SCAN_ENABLED=false)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool.Start()
	log.Printf("Worker pool started with %d workers, polling every %s", concurrency, pollInterval)

	<-ctx.Done()
	log.Println("Shutting down worker pools...")
	pool.Stop()
	if scanPool != nil {
		scanPool.Stop()
	}
	log.Println("Worker pools stopped")
}

func runPreMigrations(db *gorm.DB) error {
	stmts := []struct {
		check string
		stmts []string
	}{
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='security_scans' AND column_name='revision_id'`,
			stmts: []string{
				`ALTER TABLE security_scans DROP COLUMN IF EXISTS revision_id`,
			},
		},
	}
	for _, m := range stmts {
		var exists int
		db.Raw(m.check).Scan(&exists)
		if exists != 1 {
			continue
		}
		for _, stmt := range m.stmts {
			if err := db.Exec(stmt).Error; err != nil {
				return fmt.Errorf("pre-migration failed (%s): %w", stmt, err)
			}
		}
	}
	return nil
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
