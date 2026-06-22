package audit

import (
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// Service is the read/query layer for admin_audit_logs (the write path lives on
// the package-level Logger so it can be a side-effect of management handlers).
type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// Filter narrows an audit-log query. Empty string fields and nil time pointers
// are ignored. From/To are inclusive lower/upper bounds on created_at.
type Filter struct {
	Action     string
	ActorID    string
	TargetType string
	From       *time.Time
	To         *time.Time
}

// List returns a page of audit entries (newest first) matching the filter,
// together with the total matching count for pagination. page is 1-based;
// pageSize is clamped to [1, 200] with a default of 20.
func (s *Service) List(f Filter, page, pageSize int) ([]models.AdminAuditLog, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 200 {
		pageSize = 200
	}

	q := s.db.Model(&models.AdminAuditLog{})
	if f.Action != "" {
		q = q.Where("action = ?", f.Action)
	}
	if f.ActorID != "" {
		q = q.Where("actor_id = ?", f.ActorID)
	}
	if f.TargetType != "" {
		q = q.Where("target_type = ?", f.TargetType)
	}
	if f.From != nil {
		q = q.Where("created_at >= ?", *f.From)
	}
	if f.To != nil {
		q = q.Where("created_at <= ?", *f.To)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var logs []models.AdminAuditLog
	if err := q.Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&logs).Error; err != nil {
		return nil, 0, err
	}

	return logs, total, nil
}
