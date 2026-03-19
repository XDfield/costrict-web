package services

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

var (
	ErrDeviceAlreadyRegistered = errors.New("device already registered")
	ErrDeviceNotFound          = errors.New("device not found")
)

type DeviceService struct {
	DB *gorm.DB
}

type RegisterDeviceRequest struct {
	DeviceID    string `json:"deviceId" binding:"required"`
	DisplayName string `json:"displayName" binding:"required"`
	Platform    string `json:"platform" binding:"required"`
	Version     string `json:"version" binding:"required"`
	WorkspaceID string `json:"workspaceId"`
}

type UpdateDeviceRequest struct {
	DisplayName string `json:"displayName"`
	WorkspaceID string `json:"workspaceId"`
}

func generateDeviceToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b), nil
}

func (s *DeviceService) RegisterDevice(userID string, req RegisterDeviceRequest) (*models.Device, string, error) {
	var existing models.Device
	result := s.DB.Where("device_id = ?", req.DeviceID).First(&existing)
	if result.Error == nil {
		return nil, "", ErrDeviceAlreadyRegistered
	}
	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil, "", result.Error
	}

	token, err := generateDeviceToken()
	if err != nil {
		return nil, "", err
	}

	device := &models.Device{
		DeviceID:    req.DeviceID,
		DisplayName: req.DisplayName,
		Platform:    req.Platform,
		Version:     req.Version,
		UserID:      userID,
		WorkspaceID: req.WorkspaceID,
		Status:      "offline",
		Token:       token,
	}

	if err := s.DB.Create(device).Error; err != nil {
		return nil, "", err
	}

	return device, token, nil
}

func (s *DeviceService) GetDevice(deviceID, userID string) (*models.Device, error) {
	var device models.Device
	result := s.DB.Where("device_id = ? AND user_id = ?", deviceID, userID).First(&device)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrDeviceNotFound
		}
		return nil, result.Error
	}
	return &device, nil
}

// GetDeviceByID 通过设备 UUID 获取设备
func (s *DeviceService) GetDeviceByID(id, userID string) (*models.Device, error) {
	var device models.Device
	result := s.DB.Where("id = ? AND user_id = ?", id, userID).First(&device)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrDeviceNotFound
		}
		return nil, result.Error
	}
	return &device, nil
}

func (s *DeviceService) ListDevices(userID string) ([]models.Device, error) {
	var devices []models.Device
	if err := s.DB.Where("user_id = ?", userID).Find(&devices).Error; err != nil {
		return nil, err
	}
	return devices, nil
}

func (s *DeviceService) ListWorkspaceDevices(workspaceID, userID string, page, pageSize int) ([]models.Device, int64, error) {
	var total int64
	query := s.DB.Model(&models.Device{}).Where("workspace_id = ?", workspaceID)
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	offset := (page - 1) * pageSize
	var devices []models.Device
	if err := query.Order("created_at DESC").Limit(pageSize).Offset(offset).Find(&devices).Error; err != nil {
		return nil, 0, err
	}
	return devices, total, nil
}

func (s *DeviceService) UpdateDevice(deviceID, userID string, req UpdateDeviceRequest) (*models.Device, error) {
	device, err := s.GetDevice(deviceID, userID)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{}
	if req.DisplayName != "" {
		updates["display_name"] = req.DisplayName
	}
	if req.WorkspaceID != "" {
		updates["workspace_id"] = req.WorkspaceID
	}

	if len(updates) > 0 {
		if err := s.DB.Model(device).Updates(updates).Error; err != nil {
			return nil, err
		}
	}

	return device, nil
}

func (s *DeviceService) DeleteDevice(deviceID, userID string) error {
	device, err := s.GetDevice(deviceID, userID)
	if err != nil {
		return err
	}
	return s.DB.Delete(device).Error
}

func (s *DeviceService) RotateToken(deviceID, userID string) (string, time.Time, error) {
	device, err := s.GetDevice(deviceID, userID)
	if err != nil {
		return "", time.Time{}, err
	}

	token, err := generateDeviceToken()
	if err != nil {
		return "", time.Time{}, err
	}

	now := time.Now()
	if err := s.DB.Model(device).Updates(map[string]any{
		"token":            token,
		"token_rotated_at": now,
	}).Error; err != nil {
		return "", time.Time{}, err
	}

	return token, now, nil
}

func (s *DeviceService) VerifyDeviceOwnership(deviceID, userID string) (*models.Device, error) {
	return s.GetDevice(deviceID, userID)
}

func (s *DeviceService) VerifyDeviceToken(token string) (*models.Device, error) {
	var device models.Device
	result := s.DB.Where("token = ?", token).First(&device)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrDeviceNotFound
		}
		return nil, result.Error
	}
	return &device, nil
}

func (s *DeviceService) SetOnline(deviceID string) error {
	now := time.Now()
	return s.DB.Model(&models.Device{}).
		Where("device_id = ?", deviceID).
		Updates(map[string]any{
			"status":            "online",
			"last_connected_at": now,
		}).Error
}

func (s *DeviceService) SetOffline(deviceID string) error {
	return s.DB.Model(&models.Device{}).
		Where("device_id = ?", deviceID).
		Update("status", "offline").Error
}

func (s *DeviceService) UpdateLastSeen(deviceID string) error {
	now := time.Now()
	return s.DB.Model(&models.Device{}).
		Where("device_id = ?", deviceID).
		Update("last_seen_at", now).Error
}
