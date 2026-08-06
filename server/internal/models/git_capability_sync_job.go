package models

import "time"

const (
	GitCapabilitySyncJobStatusPending = "pending"
	GitCapabilitySyncJobStatusRunning = "running"
	GitCapabilitySyncJobStatusSuccess = "success"
	GitCapabilitySyncJobStatusFailed  = "failed"
)

// Delivery-id prefixes for the jobs this platform enqueues itself.
//
// A webhook delivery carries Gitea's own X-Gitea-Delivery value (a UUID), so
// the absence of a prefix is what identifies a push. These constants exist
// because the value is now read as well as written: the revision writer
// classifies a projection's `source` from the job that authorized it
// (services.GitRevisionSourceForDelivery), and a producer that spelled its
// prefix independently would silently label every one of its projections
// "push".
const (
	GitCapabilitySyncDeliveryPrefixReconcile = "reconcile:"
	GitCapabilitySyncDeliveryPrefixManual    = "manual:"
)

// GitCapabilitySyncJob is a durable, idempotent record of one Gitea push
// delivery that must be reflected in the Git-backed capability index. The
// webhook handler only inserts this row; a dedicated worker owns state
// transitions and all capability_items updates.
//
// Mirrors migration 20260803020000_create_git_capability_sync_jobs.sql.
type GitCapabilitySyncJob struct {
	ID            string     `gorm:"column:id;primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	GitServerID   string     `gorm:"column:git_server_id;type:varchar(64);not null;uniqueIndex:uq_git_capability_sync_jobs_delivery,priority:1" json:"git_server_id"`
	DeliveryID    string     `gorm:"column:delivery_id;type:varchar(128);not null;uniqueIndex:uq_git_capability_sync_jobs_delivery,priority:2" json:"delivery_id"`
	RepoID        int64      `gorm:"column:repo_id;not null" json:"repo_id"`
	RepoFullName  string     `gorm:"column:repo_full_name;type:text;not null" json:"repo_full_name"`
	DefaultBranch string     `gorm:"column:default_branch;type:text;not null" json:"default_branch"`
	Ref           string     `gorm:"column:ref;type:text;not null" json:"ref"`
	BeforeSHA     string     `gorm:"column:before_sha;type:text;not null;default:''" json:"before_sha"`
	AfterSHA      string     `gorm:"column:after_sha;type:text;not null" json:"after_sha"`
	Status        string     `gorm:"column:status;type:varchar(32);not null;default:'pending';index" json:"status"`
	RetryCount    int        `gorm:"column:retry_count;not null;default:0" json:"retry_count"`
	MaxAttempts   int        `gorm:"column:max_attempts;not null;default:3" json:"max_attempts"`
	LastError     *string    `gorm:"column:last_error;type:text" json:"last_error,omitempty"`
	ScheduledAt   time.Time  `gorm:"column:scheduled_at;type:timestamptz;not null;index" json:"scheduled_at"`
	StartedAt     *time.Time `gorm:"column:started_at;type:timestamptz" json:"started_at,omitempty"`
	LeaseToken    string     `gorm:"column:lease_token;type:varchar(36);not null;default:''" json:"-"`
	FinishedAt    *time.Time `gorm:"column:finished_at;type:timestamptz" json:"finished_at,omitempty"`
	CreatedAt     time.Time  `gorm:"column:created_at;type:timestamptz;not null;default:now()" json:"created_at"`
}

// TableName pins the production table name.
func (GitCapabilitySyncJob) TableName() string { return "git_capability_sync_jobs" }
