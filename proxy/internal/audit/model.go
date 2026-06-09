package audit

import (
	"crypto/rand"
	"fmt"
	"time"

	"gorm.io/gorm"
)

type AuditLog struct {
	ID                 string     `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	UserID             string     `gorm:"type:varchar(191);not null;index:idx_audit_user_time" json:"user_id"`
	UserName           string     `gorm:"type:varchar(255)" json:"user_name"`
	UserSub            string     `gorm:"type:varchar(255)" json:"user_sub"`
	DeviceID           string     `gorm:"type:varchar(191)" json:"device_id"`
	ClientIP           string     `gorm:"type:varchar(45);not null" json:"client_ip"`
	ClientType         string     `gorm:"type:varchar(32);not null;default:''" json:"client_type"`
	ApiPath            string     `gorm:"type:varchar(512);not null;index:idx_audit_path_time" json:"api_path"`
	Method             string     `gorm:"type:varchar(16);not null" json:"method"`
	SessionID          string     `gorm:"type:varchar(191);index:idx_audit_session" json:"session_id"`
	ConversationID     string     `gorm:"type:varchar(191)" json:"conversation_id"`
	StatusCode         int        `json:"status_code"`
	RequestSummary     string     `gorm:"type:text" json:"request_summary"`
	FilesCount         int        `gorm:"default:0" json:"files_count"`
	ToolsCount         int        `gorm:"default:0" json:"tools_count"`
	CodeBlocksTotal    int        `gorm:"default:0" json:"code_blocks_total"`
	CodeBlocksFiltered int        `gorm:"default:0" json:"code_blocks_filtered"`
	Filtered           bool       `gorm:"default:false;index:idx_audit_filtered" json:"filtered"`
	LatencyMs          int        `json:"latency_ms"`
	IsSse              bool       `gorm:"default:false" json:"is_sse"`
	CreatedAt          time.Time  `gorm:"not null;default:now();index:idx_audit_user_time,index:idx_audit_path_time" json:"created_at"`
	Files              []AuditFile `gorm:"foreignKey:AuditID" json:"files,omitempty"`
	Tools              []AuditTool `gorm:"foreignKey:AuditID" json:"tools,omitempty"`
}

type AuditFile struct {
	ID         string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	AuditID    string    `gorm:"type:uuid;not null;index:idx_audit_files_audit" json:"audit_id"`
	FilePath   string    `gorm:"type:varchar(1024);not null;index:idx_audit_files_path" json:"file_path"`
	AccessType string    `gorm:"type:varchar(32);not null" json:"access_type"`
	CreatedAt  time.Time `gorm:"not null;default:now()" json:"created_at"`
}

type AuditTool struct {
	ID        string    `gorm:"primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	AuditID   string    `gorm:"type:uuid;not null;index:idx_audit_tools_audit" json:"audit_id"`
	ToolName  string    `gorm:"type:varchar(191);not null;index:idx_audit_tools_name" json:"tool_name"`
	Input     string    `gorm:"type:text" json:"input"`
	Filtered  bool      `gorm:"default:false" json:"filtered"`
	CreatedAt time.Time `gorm:"not null;default:now()" json:"created_at"`
}

func (AuditLog) TableName() string    { return "audit_logs" }
func (AuditFile) TableName() string   { return "audit_files" }
func (AuditTool) TableName() string   { return "audit_tools" }

func NewAuditLog() *AuditLog {
	b := make([]byte, 16)
	rand.Read(b)
	return &AuditLog{
		ID: fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]),
	}
}

type AuditStore interface {
	InsertBatch(entries []*AuditLog) error
	CleanBefore(before time.Time) error
}

type GormAuditStore struct {
	db *gorm.DB
}
