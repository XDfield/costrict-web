package models

import (
	"time"

	"gorm.io/datatypes"
)

// CapabilityImportJob tracks an admin-triggered catalog-bundle import through its
// dry-run → confirm → real-import lifecycle, and doubles as the import history /
// audit trail. It is executed asynchronously by the leader-elected import runner
// (internal/adminimport), NOT in the HTTP request goroutine — mirroring the
// ScanJob/SyncJob queue-and-poller shape so a pod restart can recover in-flight
// work (see the task design doc, "执行模型（方案 D）").
type CapabilityImportJob struct {
	ID             string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	SourceKind     string         `gorm:"not null;default:'url'" json:"sourceKind"` // url | upload
	SourceURL      string         `json:"sourceUrl,omitempty"`                      // set when sourceKind=url
	Filename       string         `json:"filename"`                                 // original filename (upload) or URL tail (url); display only
	StorageBackend string         `gorm:"not null;default:''" json:"-"`             // set when sourceKind=upload: local | s3
	StorageKey     string         `json:"-"`                                        // set when sourceKind=upload: bundle object key in StorageBackend
	FileSize       int64          `gorm:"default:0" json:"fileSize"`
	Status         string         `gorm:"not null;default:'pending';index" json:"status"` // pending | running | previewed | success | failed | expired
	DryRun         bool           `gorm:"not null;default:true" json:"dryRun"`            // true=dry-run preview phase; false=real import phase
	Reparse        bool           `gorm:"not null;default:false" json:"reparse"`          // force full re-parse (IngestOptions.Reparse)
	TriggerUser    string         `gorm:"not null" json:"triggerUser"`
	Result         datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"result" swaggertype:"object"` // serialized adminimport.ImportResult
	ErrorMessage   string         `gorm:"type:text" json:"errorMessage,omitempty"`
	RetryCount     int            `gorm:"default:0" json:"retryCount"`
	MaxAttempts    int            `gorm:"default:3" json:"maxAttempts"`
	ScheduledAt    time.Time      `gorm:"index" json:"scheduledAt"`
	StartedAt      *time.Time     `json:"startedAt,omitempty"`
	FinishedAt     *time.Time     `json:"finishedAt,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
}
