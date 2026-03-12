package services

import (
	"errors"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var ErrJobAlreadyQueued = errors.New("a sync job is already pending or running for this registry")

type JobService struct {
	DB *gorm.DB
}

type EnqueueOptions struct {
	Priority    int
	ScheduledAt time.Time
	DryRun      bool
	MaxAttempts int
}

func (j *JobService) Enqueue(registryID, triggerType, triggerUser string, opts EnqueueOptions) (*models.SyncJob, error) {
	var count int64
	j.DB.Model(&models.SyncJob{}).
		Where("registry_id = ? AND status IN ('pending', 'running')", registryID).
		Count(&count)

	if count > 0 {
		if triggerType == "scheduled" {
			return nil, nil
		}
		return nil, ErrJobAlreadyQueued
	}

	if opts.Priority == 0 {
		opts.Priority = 5
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 3
	}
	if opts.ScheduledAt.IsZero() {
		opts.ScheduledAt = time.Now()
	}

	payload, _ := datatypes.JSONMap{"dryRun": opts.DryRun}.MarshalJSON()

	job := &models.SyncJob{
		ID:          uuid.New().String(),
		RegistryID:  registryID,
		TriggerType: triggerType,
		TriggerUser: triggerUser,
		Priority:    opts.Priority,
		Status:      "pending",
		Payload:     datatypes.JSON(payload),
		MaxAttempts: opts.MaxAttempts,
		ScheduledAt: opts.ScheduledAt,
	}

	if err := j.DB.Create(job).Error; err != nil {
		return nil, err
	}
	return job, nil
}

func (j *JobService) Cancel(jobID, operatorID string) error {
	result := j.DB.Model(&models.SyncJob{}).
		Where("id = ? AND status = 'pending'", jobID).
		Updates(map[string]any{"status": "cancelled"})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("job not found or not in pending state")
	}
	return nil
}

func (j *JobService) CancelByRegistry(registryID string) error {
	return j.DB.Model(&models.SyncJob{}).
		Where("registry_id = ? AND status = 'pending'", registryID).
		Updates(map[string]any{"status": "cancelled"}).Error
}

func (j *JobService) GetPendingCount(registryID string) (int64, error) {
	var count int64
	err := j.DB.Model(&models.SyncJob{}).
		Where("registry_id = ? AND status IN ('pending', 'running')", registryID).
		Count(&count).Error
	return count, err
}

func (j *JobService) ListJobs(registryID string, page, pageSize int) ([]models.SyncJob, int64, error) {
	var jobs []models.SyncJob
	var total int64

	query := j.DB.Model(&models.SyncJob{})
	if registryID != "" {
		query = query.Where("registry_id = ?", registryID)
	}

	query.Count(&total)
	err := query.Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&jobs).Error

	return jobs, total, err
}
