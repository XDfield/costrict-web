package main

import (
	"context"
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
)

func main() {
	cfg := config.Load()

	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}

	err = db.AutoMigrate(
		&models.SyncJob{},
		&models.SyncLog{},
		&models.CapabilityRegistry{},
		&models.CapabilityItem{},
		&models.CapabilityVersion{},
		&models.CapabilityAsset{},
	)
	if err != nil {
		log.Fatalf("Failed to migrate database: %v", err)
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
