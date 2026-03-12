package models

import (
	"time"

	"gorm.io/datatypes"
)

// Repository represents a repository
type Repository struct {
	ID          string    `gorm:"type:uuid;primaryKey" json:"id"`
	Name        string    `gorm:"type:varchar(255);not null;uniqueIndex" json:"name"`
	DisplayName string    `gorm:"type:varchar(255)" json:"displayName"`
	Description string    `gorm:"type:text" json:"description"`
	Visibility  string    `gorm:"type:varchar(32);default:'private'" json:"visibility"` // public | private
	RepoType    string    `gorm:"type:varchar(32);default:'normal'" json:"repoType"`    // normal | sync
	OwnerID     string    `gorm:"type:varchar(191);not null" json:"ownerId"`
	CreatedAt   time.Time `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt   time.Time `gorm:"autoUpdateTime" json:"updatedAt"`

	Members []RepoMember `gorm:"foreignKey:RepoID" json:"members,omitempty"`
}

// RepoMember represents a user's membership in a repository
type RepoMember struct {
	ID        string    `gorm:"type:uuid;primaryKey" json:"id"`
	RepoID    string    `gorm:"type:uuid;not null;index" json:"repoId"`
	UserID    string    `gorm:"type:varchar(191);not null" json:"userId"`
	Username  string    `gorm:"type:varchar(255)" json:"username"`
	Role      string    `gorm:"type:varchar(32);default:'member'" json:"role"` // owner | admin | member
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`
}

// RepoInvitation represents an invitation to join a repository
type RepoInvitation struct {
	ID              string     `gorm:"type:uuid;primaryKey" json:"id"`
	RepoID          string     `gorm:"type:uuid;not null;index" json:"repoId"`
	InviterID       string     `gorm:"type:varchar(191);not null" json:"inviterId"`
	InviterUsername string     `gorm:"type:varchar(255)" json:"inviterUsername"`
	InviteeID       string     `gorm:"type:varchar(191);index" json:"inviteeId"`
	InviteeUsername string     `gorm:"type:varchar(255);not null" json:"inviteeUsername"`
	Role            string     `gorm:"type:varchar(32);default:'member'" json:"role"`                  // admin | member
	Status          string     `gorm:"type:varchar(32);default:'pending';index" json:"status"`         // pending | accepted | declined | cancelled
	ExpiresAt       time.Time  `gorm:"not null" json:"expiresAt"`
	CreatedAt       time.Time  `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt       time.Time  `gorm:"autoUpdateTime" json:"updatedAt"`

	Repository *Repository `gorm:"foreignKey:RepoID" json:"repository,omitempty"`
}


type CapabilityRegistry struct {
	ID          string     `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	Name        string     `gorm:"not null" json:"name"`
	Description string     `json:"description"`
	SourceType  string     `gorm:"not null;default:'internal'" json:"sourceType"`
	ExternalURL    string     `json:"externalUrl"`
	ExternalBranch string     `gorm:"default:'main'" json:"externalBranch"`
	SyncEnabled    bool       `gorm:"default:false" json:"syncEnabled"`
	SyncInterval   int        `gorm:"default:3600" json:"syncInterval"`
	LastSyncedAt   *time.Time `json:"lastSyncedAt"`
	LastSyncSHA    string     `json:"lastSyncSha"`
	SyncStatus     string         `gorm:"default:'idle'" json:"syncStatus"` // idle | syncing | error | paused
	SyncConfig     datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"syncConfig" swaggertype:"object"`
	LastSyncLogID  *string        `gorm:"type:uuid" json:"lastSyncLogId"`
	Visibility string `gorm:"default:'repo'" json:"visibility"`
	RepoID     string `json:"repoId"`
	OwnerID    string `gorm:"not null" json:"ownerId"`
	Items       []CapabilityItem `gorm:"foreignKey:RegistryID" json:"items,omitempty"`
	LastSyncLog *SyncLog         `gorm:"foreignKey:LastSyncLogID;references:ID" json:"lastSyncLog,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type SyncJob struct {
	ID          string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	RegistryID  string         `gorm:"not null;index" json:"registryId"`
	TriggerType string         `gorm:"not null" json:"triggerType"` // scheduled | manual | webhook
	TriggerUser string         `json:"triggerUser"`
	Priority    int            `gorm:"not null;default:5" json:"priority"`
	Status      string         `gorm:"not null;default:'pending';index" json:"status"` // pending | running | success | failed | cancelled
	Payload     datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"payload" swaggertype:"object"`
	RetryCount  int            `gorm:"default:0" json:"retryCount"`
	MaxAttempts int            `gorm:"default:3" json:"maxAttempts"`
	LastError   string         `gorm:"type:text" json:"lastError"`
	ScheduledAt time.Time      `gorm:"not null;index" json:"scheduledAt"`
	StartedAt   *time.Time     `json:"startedAt"`
	FinishedAt  *time.Time     `json:"finishedAt"`
	SyncLogID   *string        `json:"syncLogId"`
	CreatedAt   time.Time      `json:"createdAt"`

	Registry *CapabilityRegistry `gorm:"foreignKey:RegistryID;constraint:false" json:"registry,omitempty"`
}

type SyncLog struct {
	ID           string     `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	RegistryID   string     `gorm:"not null;index" json:"registryId"`
	TriggerType  string     `gorm:"not null" json:"triggerType"` // scheduled | manual | webhook
	TriggerUser  string     `json:"triggerUser"`
	Status       string     `gorm:"not null;default:'running'" json:"status"` // running | success | failed | cancelled
	CommitSHA    string     `json:"commitSha"`
	PreviousSHA  string     `json:"previousSha"`
	TotalItems   int        `gorm:"default:0" json:"totalItems"`
	AddedItems   int        `gorm:"default:0" json:"addedItems"`
	UpdatedItems int        `gorm:"default:0" json:"updatedItems"`
	DeletedItems int        `gorm:"default:0" json:"deletedItems"`
	SkippedItems int        `gorm:"default:0" json:"skippedItems"`
	FailedItems  int        `gorm:"default:0" json:"failedItems"`
	ErrorMessage string     `gorm:"type:text" json:"errorMessage"`
	DurationMs   int64      `json:"durationMs"`
	StartedAt    time.Time  `gorm:"not null" json:"startedAt"`
	FinishedAt   *time.Time `json:"finishedAt"`
	CreatedAt    time.Time  `json:"createdAt"`

	Registry *CapabilityRegistry `gorm:"foreignKey:RegistryID;constraint:false" json:"registry,omitempty"`
}

type CapabilityItem struct {
	ID             string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	RegistryID     string         `gorm:"not null;uniqueIndex:idx_item_slug,composite:registry_id,item_type,slug" json:"registryId"`
	Slug           string         `gorm:"not null;uniqueIndex:idx_item_slug,composite:registry_id,item_type,slug" json:"slug"`
	ItemType       string         `gorm:"not null;uniqueIndex:idx_item_slug,composite:registry_id,item_type,slug" json:"itemType"`
	Name           string         `gorm:"not null" json:"name"`
	Description    string         `json:"description"`
	Category       string         `json:"category"`
	Version        string         `gorm:"default:'1.0.0'" json:"version"`
	Content        string         `gorm:"type:text" json:"content"`
	Metadata       datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"metadata" swaggertype:"object"`
	SourcePath     string         `json:"sourcePath"`
	SourceSHA      string         `json:"sourceSha"`
	InstallCount   int            `gorm:"default:0" json:"installCount"`
	Status         string         `gorm:"default:'active'" json:"status"`
	SecurityStatus string         `gorm:"default:'unscanned'" json:"securityStatus"`
	LastScanID     *string        `json:"lastScanId,omitempty"`
	CreatedBy string `gorm:"not null" json:"createdBy"`
	UpdatedBy string `json:"updatedBy"`
	Registry  *CapabilityRegistry  `gorm:"foreignKey:RegistryID" json:"registry,omitempty"`
	Versions  []CapabilityVersion  `gorm:"foreignKey:ItemID" json:"versions,omitempty"`
	Assets    []CapabilityAsset    `gorm:"foreignKey:ItemID" json:"assets,omitempty"`
	Artifacts []CapabilityArtifact `gorm:"foreignKey:ItemID" json:"artifacts,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`

	// Vector embedding for semantic search
	Embedding         *string    `gorm:"type:vector(1024)" json:"-"`
	ExperienceScore   float64    `gorm:"default:0" json:"experienceScore"`
	EmbeddingUpdatedAt *time.Time `json:"embeddingUpdatedAt"`
}

type CapabilityVersion struct {
	ID        string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	ItemID    string         `gorm:"not null;index" json:"itemId"`
	Revision  int            `gorm:"not null;column:revision" json:"revision"`
	Content   string         `gorm:"type:text;not null" json:"content"`
	Metadata  datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"metadata" swaggertype:"object"`
	CommitMsg string         `json:"commitMsg"`
	CreatedBy string         `gorm:"not null" json:"createdBy"`
	CreatedAt time.Time      `json:"createdAt"`
}

type CapabilityAsset struct {
	ID             string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	ItemID         string    `gorm:"not null;index" json:"itemId"`
	RelPath        string    `gorm:"not null" json:"relPath"`
	TextContent    *string   `gorm:"type:text" json:"textContent,omitempty"`
	StorageBackend string    `gorm:"default:'local'" json:"storageBackend"`
	StorageKey     string    `json:"storageKey,omitempty"`
	MimeType       string    `json:"mimeType"`
	FileSize       int64     `gorm:"default:0" json:"fileSize"`
	ContentSHA     string    `json:"contentSha"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type CapabilityArtifact struct {
	ID              string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	ItemID          string    `gorm:"not null;index" json:"itemId"`
	Filename        string    `gorm:"not null" json:"filename"`
	FileSize        int64     `gorm:"not null" json:"fileSize"`
	ChecksumSHA256  string    `gorm:"not null" json:"checksumSha256"`
	MimeType        string    `json:"mimeType"`
	StorageBackend  string    `gorm:"default:'local'" json:"storageBackend"`
	StorageKey      string    `gorm:"not null" json:"storageKey"`
	ArtifactVersion string    `gorm:"not null" json:"artifactVersion"`
	IsLatest        bool      `gorm:"default:false" json:"isLatest"`
	SourceType      string    `gorm:"default:'upload'" json:"sourceType"`
	DownloadCount   int       `gorm:"default:0" json:"downloadCount"`
	UploadedBy      string    `gorm:"not null" json:"uploadedBy"`
	CreatedAt       time.Time `json:"createdAt"`
}

type SecurityScan struct {
	ID          string         `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	ItemID      string         `gorm:"not null;index" json:"itemId"`
	RevisionID  string         `gorm:"not null" json:"revisionId"`
	TriggerType string         `gorm:"not null" json:"triggerType"`
	Status      string         `gorm:"not null;default:'pending'" json:"status"`
	RiskLevel   string         `json:"riskLevel"`
	Verdict     string         `json:"verdict"`
	RedFlags    datatypes.JSON `gorm:"type:jsonb;default:'[]'" json:"redFlags"`
	Permissions datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"permissions"`
	Report      datatypes.JSON `gorm:"type:jsonb;default:'{}'" json:"report"`
	ErrorList   datatypes.JSON `gorm:"type:jsonb;default:'[]'" json:"errorList"`
	ScannedBy   string         `json:"scannedBy"`
	CreatedAt   time.Time      `json:"createdAt"`
	FinishedAt  *time.Time     `json:"finishedAt,omitempty"`
}
