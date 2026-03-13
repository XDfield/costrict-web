package services

import (
	"errors"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var ErrScanJobAlreadyQueued = errors.New("a scan job is already pending or running for this item")

type ScanJobService struct {
	DB *gorm.DB
}

type ScanEnqueueOptions struct {
	Priority    int
	ScheduledAt time.Time
	MaxAttempts int
}

func (s *ScanJobService) Enqueue(itemID string, itemRevision int, triggerType, triggerUser string, opts ScanEnqueueOptions) (*models.ScanJob, error) {
	var count int64
	s.DB.Model(&models.ScanJob{}).
		Where("item_id = ? AND status IN ('pending', 'running')", itemID).
		Count(&count)

	if count > 0 {
		if triggerType == "create" || triggerType == "update" || triggerType == "sync" {
			return nil, nil
		}
		return nil, ErrScanJobAlreadyQueued
	}

	if opts.Priority == 0 {
		opts.Priority = 5
	}
	if opts.MaxAttempts == 0 {
		opts.MaxAttempts = 2
	}
	if opts.ScheduledAt.IsZero() {
		opts.ScheduledAt = time.Now()
	}

	job := &models.ScanJob{
		ID:           uuid.New().String(),
		ItemID:       itemID,
		ItemRevision: itemRevision,
		TriggerType:  triggerType,
		TriggerUser:  triggerUser,
		Priority:     opts.Priority,
		Status:       "pending",
		MaxAttempts:  opts.MaxAttempts,
		ScheduledAt:  opts.ScheduledAt,
	}

	if err := s.DB.Create(job).Error; err != nil {
		return nil, err
	}
	return job, nil
}

func (s *ScanJobService) Cancel(jobID string) error {
	result := s.DB.Model(&models.ScanJob{}).
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

func (s *ScanJobService) GetActiveCount(itemID string) (int64, error) {
	var count int64
	err := s.DB.Model(&models.ScanJob{}).
		Where("item_id = ? AND status IN ('pending', 'running')", itemID).
		Count(&count).Error
	return count, err
}

func (s *ScanJobService) ListJobs(itemID string, page, pageSize int) ([]models.ScanJob, int64, error) {
	var jobs []models.ScanJob
	var total int64

	query := s.DB.Model(&models.ScanJob{})
	if itemID != "" {
		query = query.Where("item_id = ?", itemID)
	}

	query.Count(&total)
	err := query.Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&jobs).Error

	return jobs, total, err
}
