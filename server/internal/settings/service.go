package settings

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const (
	// MaxKeyBytes caps the setting key length (mirrors the system_settings.key
	// VARCHAR(128) column). Keys are admin-defined feature-flag / config names.
	MaxKeyBytes = 128
	// MaxValueBytes caps the stored JSON value. The value column is JSONB with no
	// body-size middleware in front of the admin write path; 64KB is ample for a
	// feature flag / maintenance-mode payload while preventing accidental blobs.
	MaxValueBytes = 64 * 1024
)

var (
	// ErrInvalidKey is returned for an empty / over-long key.
	ErrInvalidKey = errors.New("invalid setting key")
	// ErrInvalidValue is returned when the value is not valid JSON or exceeds
	// MaxValueBytes.
	ErrInvalidValue = errors.New("invalid setting value")
)

// Service is the data-access layer for the global system_settings KV table.
type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// GetAll returns every system setting as a key→raw-JSON map (sorted by key).
func (s *Service) GetAll() (map[string]json.RawMessage, error) {
	var rows []models.SystemSetting
	if err := s.db.Order("key ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(rows))
	for _, row := range rows {
		// datatypes.JSON is a []byte alias; copy into json.RawMessage for the API.
		out[row.Key] = json.RawMessage(row.Value)
	}
	return out, nil
}

// Set upserts a single key with its JSON value, recording the operator. The
// value must be syntactically valid JSON and within MaxValueBytes; the key must
// be non-empty and within MaxKeyBytes.
func (s *Service) Set(key string, value json.RawMessage, operatorID string) (*models.SystemSetting, error) {
	if key == "" || len(key) > MaxKeyBytes {
		return nil, ErrInvalidKey
	}
	if len(value) == 0 {
		value = json.RawMessage("null")
	}
	if len(value) > MaxValueBytes || !json.Valid(value) {
		return nil, ErrInvalidValue
	}

	now := time.Now()
	row := models.SystemSetting{
		Key:       key,
		Value:     datatypes.JSON(value),
		UpdatedBy: operatorID,
		UpdatedAt: now,
		CreatedAt: now,
	}
	// Upsert on the primary key: insert new, or update value/updated_by/updated_at
	// on conflict (created_at preserved by omitting it from the update set).
	if err := s.db.Save(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}
