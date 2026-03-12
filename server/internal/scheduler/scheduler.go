package scheduler

import (
	"log"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Scheduler struct {
	cron       gocron.Scheduler
	JobService *services.JobService
	DB         *gorm.DB
	jobMap     map[string]uuid.UUID
	mu         sync.RWMutex
}

func (s *Scheduler) Start() error {
	cron, err := gocron.NewScheduler()
	if err != nil {
		return err
	}
	s.cron = cron
	s.jobMap = make(map[string]uuid.UUID)

	var registries []models.CapabilityRegistry
	s.DB.Where("sync_enabled = true AND external_url != ''").Find(&registries)

	for i := range registries {
		if err := s.RegisterRegistry(&registries[i]); err != nil {
			log.Printf("Failed to register scheduler for registry %s: %v", registries[i].ID, err)
		}
	}

	s.cron.Start()
	log.Printf("Scheduler started with %d registries", len(registries))
	return nil
}

func (s *Scheduler) Stop() {
	if s.cron != nil {
		_ = s.cron.Shutdown()
	}
}

func (s *Scheduler) RegisterRegistry(registry *models.CapabilityRegistry) error {
	if !registry.SyncEnabled || registry.ExternalURL == "" {
		s.UnregisterRegistry(registry.ID)
		return nil
	}

	interval := registry.SyncInterval
	if interval <= 0 {
		interval = 3600
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if existingID, ok := s.jobMap[registry.ID]; ok {
		s.cron.RemoveByTags(existingID.String())
	}

	registryID := registry.ID
	jobSvc := s.JobService

	job, err := s.cron.NewJob(
		gocron.DurationJob(time.Duration(interval)*time.Second),
		gocron.NewTask(func() {
			_, err := jobSvc.Enqueue(registryID, "scheduled", "", services.EnqueueOptions{
				Priority: 5,
			})
			if err != nil {
				log.Printf("Scheduler: failed to enqueue job for registry %s: %v", registryID, err)
			}
		}),
		gocron.WithSingletonMode(gocron.LimitModeReschedule),
	)
	if err != nil {
		return err
	}

	s.jobMap[registry.ID] = job.ID()
	return nil
}

func (s *Scheduler) UnregisterRegistry(registryID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if jobID, ok := s.jobMap[registryID]; ok {
		s.cron.RemoveByTags(jobID.String())
		delete(s.jobMap, registryID)
	}
}
