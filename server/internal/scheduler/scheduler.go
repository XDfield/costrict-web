package scheduler

import (
	"context"
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

// SyncDisabled 全局禁用 sync scheduler 的周期 clone 触发。
//
// 封禁动因：GitService.Clone (services/git_service.go) 对 externalUrl 零 SSRF
// 防护，scheduler 周期触发 SyncRegistry → Clone → git.PlainClone 会向
// <externalUrl>/info/refs?service=git-upload-pack 发 HTTP GET，构成 SSRF。
// 见 secreport 20260731141243580377 (CVSS 5.3)。
//
// 回滚：将本常量改为 false 即恢复 scheduler 注册与周期触发；HTTP 触发入口
// (handlers.TriggerRegistrySync / TriggerRepoSync) 需另行解封。
const SyncDisabled = true

func (s *Scheduler) Start() error {
	if SyncDisabled {
		log.Printf("Scheduler disabled (sync SSRF mitigation; secreport 20260731141243580377)")
		return nil
	}
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

// StartLeader runs the scheduler while ctx is active and stops it when ctx is cancelled.
// It is intended for use with leader.Election so that only one replica runs the scheduler.
func (s *Scheduler) StartLeader(ctx context.Context) error {
	if err := s.Start(); err != nil {
		return err
	}
	<-ctx.Done()
	s.Stop()
	return ctx.Err()
}

func (s *Scheduler) Stop() {
	if s.cron != nil {
		_ = s.cron.Shutdown()
	}
}

func (s *Scheduler) RegisterRegistry(registry *models.CapabilityRegistry) error {
	if SyncDisabled {
		// 封禁期间静默跳过 —— 否则 Start 未初始化 s.cron，NewJob 会 panic。
		return nil
	}
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
