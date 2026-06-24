package services

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

var (
	ErrDeviceAlreadyRegistered = errors.New("device already registered")
	ErrDeviceOwnedByCaller     = errors.New("device already registered by current user")
	ErrDeviceNotFound          = errors.New("device not found")
)

// ownershipEntry caches a device ownership check result.
type ownershipEntry struct {
	valid   bool
	expires time.Time
}

const ownershipCacheTTL = 30 * time.Second

type DeviceService struct {
	DB             *gorm.DB
	ownershipCache sync.Map // key: "deviceID:userID" -> ownershipEntry
}

type RegisterDeviceRequest struct {
	DeviceID       string `json:"deviceId" binding:"required"`
	LegacyDeviceID string `json:"legacyDeviceId"`
	DisplayName    string `json:"displayName" binding:"required"`
	Platform       string `json:"platform" binding:"required"`
	Version        string `json:"version" binding:"required"`
}

type UpdateDeviceRequest struct {
	DisplayName string  `json:"displayName"`
	Description *string `json:"description"`
	Label       *string `json:"label"`
}

func generateDeviceToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b), nil
}

func (s *DeviceService) userExists(userID string) bool {
	var count int64
	s.DB.Table("users").Where("subject_id = ?", userID).Count(&count)
	return count > 0
}

func (s *DeviceService) RegisterDevice(userID string, req RegisterDeviceRequest) (*models.Device, string, error) {
	var existing models.Device
	result := s.DB.Where("device_id = ?", req.DeviceID).First(&existing)
	if result.Error == nil {
		return s.handleExistingDevice(existing, userID, req, false)
	}
	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil, "", result.Error
	}

	var softDeleted models.Device
	if err := s.DB.Unscoped().Where("device_id = ? AND deleted_at IS NOT NULL", req.DeviceID).First(&softDeleted).Error; err == nil {
		return s.restoreSoftDeletedDevice(softDeleted, userID, req, false)
	}

	if req.LegacyDeviceID != "" && req.LegacyDeviceID != req.DeviceID {
		device, token, err := s.migrateFromLegacyDeviceID(userID, req)
		if device != nil || err != nil {
			return device, token, err
		}
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
		Status:      "offline",
		Token:       token,
	}

	if err := s.DB.Create(device).Error; err != nil {
		return nil, "", err
	}

	return device, token, nil
}

func (s *DeviceService) migrateFromLegacyDeviceID(userID string, req RegisterDeviceRequest) (*models.Device, string, error) {
	var existing models.Device
	result := s.DB.Where("device_id = ?", req.LegacyDeviceID).First(&existing)
	if result.Error == nil {
		return s.handleExistingDevice(existing, userID, req, true)
	}
	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil, "", result.Error
	}

	var softDeleted models.Device
	if err := s.DB.Unscoped().Where("device_id = ? AND deleted_at IS NOT NULL", req.LegacyDeviceID).First(&softDeleted).Error; err == nil {
		return s.restoreSoftDeletedDevice(softDeleted, userID, req, true)
	}

	return nil, "", nil
}

func (s *DeviceService) handleExistingDevice(existing models.Device, userID string, req RegisterDeviceRequest, isLegacyMigration bool) (*models.Device, string, error) {
	if existing.UserID == userID {
		if isLegacyMigration {
			return s.migrateDeviceID(existing, userID, req)
		}
		return &existing, existing.Token, ErrDeviceOwnedByCaller
	}
	if !s.userExists(existing.UserID) {
		return s.rebindDevice(existing, userID, req, isLegacyMigration)
	}
	if isLegacyMigration && req.DeviceID != existing.DeviceID {
		return s.createDeviceFromLegacyConflict(userID, req)
	}
	return nil, "", ErrDeviceAlreadyRegistered
}

func (s *DeviceService) restoreSoftDeletedDevice(softDeleted models.Device, userID string, req RegisterDeviceRequest, isLegacyMigration bool) (*models.Device, string, error) {
	token, err := generateDeviceToken()
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	updates := map[string]any{
		"display_name":     req.DisplayName,
		"platform":         req.Platform,
		"version":          req.Version,
		"user_id":          userID,
		"token":            token,
		"token_rotated_at": nil,
		"status":           "offline",
		"deleted_at":       nil,
		"updated_at":       now,
	}
	if isLegacyMigration {
		updates["device_id"] = req.DeviceID
	}
	if err := s.DB.Unscoped().Model(&softDeleted).Updates(updates).Error; err != nil {
		return nil, "", err
	}
	s.ownershipCache.Delete(softDeleted.DeviceID + ":" + softDeleted.UserID)
	s.DB.Where("device_id = ?", req.DeviceID).First(&softDeleted)
	return &softDeleted, token, nil
}

func (s *DeviceService) migrateDeviceID(existing models.Device, userID string, req RegisterDeviceRequest) (*models.Device, string, error) {
	token, err := generateDeviceToken()
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	if err := s.DB.Model(&models.Device{}).Where("device_id = ?", req.LegacyDeviceID).Updates(map[string]any{
		"device_id":        req.DeviceID,
		"display_name":     req.DisplayName,
		"platform":         req.Platform,
		"version":          req.Version,
		"token":            token,
		"token_rotated_at": nil,
		"status":           "offline",
		"updated_at":       now,
	}).Error; err != nil {
		return nil, "", err
	}
	s.ownershipCache.Delete(req.LegacyDeviceID + ":" + userID)
	s.DB.Where("device_id = ?", req.DeviceID).First(&existing)
	return &existing, token, nil
}

func (s *DeviceService) createDeviceFromLegacyConflict(userID string, req RegisterDeviceRequest) (*models.Device, string, error) {
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
		Status:      "offline",
		Token:       token,
	}
	if err := s.DB.Create(device).Error; err != nil {
		return nil, "", err
	}
	return device, token, nil
}

func (s *DeviceService) rebindDevice(existing models.Device, userID string, req RegisterDeviceRequest, isLegacyMigration bool) (*models.Device, string, error) {
	token, err := generateDeviceToken()
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	lookupID := existing.DeviceID
	updates := map[string]any{
		"display_name":     req.DisplayName,
		"platform":         req.Platform,
		"version":          req.Version,
		"user_id":          userID,
		"token":            token,
		"token_rotated_at": nil,
		"status":           "offline",
		"updated_at":       now,
	}
	if isLegacyMigration {
		updates["device_id"] = req.DeviceID
	}
	if err := s.DB.Model(&models.Device{}).Where("device_id = ?", lookupID).Updates(updates).Error; err != nil {
		return nil, "", err
	}
	s.ownershipCache.Delete(lookupID + ":" + existing.UserID)
	s.DB.Where("device_id = ?", req.DeviceID).First(&existing)
	return &existing, token, nil
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
	var workspace models.Workspace
	if err := s.DB.Where("id = ? AND user_id = ?", workspaceID, userID).First(&workspace).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return []models.Device{}, 0, nil
		}
		return nil, 0, err
	}

	if workspace.DeviceID == "" {
		return []models.Device{}, 0, nil
	}

	var device models.Device
	if err := s.DB.Where("id = ? AND user_id = ?", workspace.DeviceID, userID).First(&device).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return []models.Device{}, 0, nil
		}
		return nil, 0, err
	}

	return []models.Device{device}, 1, nil
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
	if req.Description != nil {
		updates["description"] = *req.Description
	}
	if req.Label != nil {
		updates["label"] = *req.Label
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
	// Invalidate ownership cache
	s.ownershipCache.Delete(deviceID + ":" + userID)
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
	cacheKey := deviceID + ":" + userID

	// Check cache first
	if v, ok := s.ownershipCache.Load(cacheKey); ok {
		entry := v.(ownershipEntry)
		if time.Now().Before(entry.expires) {
			if entry.valid {
				// Cache hit (positive) — still need to return a device, do a cheap DB lookup
				// But since we know it's valid, this is just for the return value.
				// For hot path optimization, we return a minimal device to avoid the DB call.
				return &models.Device{DeviceID: deviceID, UserID: userID}, nil
			}
			return nil, ErrDeviceNotFound
		}
		// Expired, fall through to DB
		s.ownershipCache.Delete(cacheKey)
	}

	device, err := s.GetDevice(deviceID, userID)
	if err != nil {
		// Cache negative result too (prevents repeated DB queries for invalid pairs)
		s.ownershipCache.Store(cacheKey, ownershipEntry{valid: false, expires: time.Now().Add(ownershipCacheTTL)})
		return nil, err
	}

	s.ownershipCache.Store(cacheKey, ownershipEntry{valid: true, expires: time.Now().Add(ownershipCacheTTL)})
	return device, nil
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

// SetOfflineReason marks a device offline and logs WHY it went offline.
// 排查专用：所有让设备下线的路径都应改用本方法，传入语义化 reason，
// 这样日志里就能看到“设备为什么下线”。仅在 online -> offline 转变时打印，
// 避免对已离线设备重复刷屏。
func (s *DeviceService) SetOfflineReason(deviceID, reason string) error {
	var cur models.Device
	err := s.DB.Select("device_id, status").Where("device_id = ?", deviceID).First(&cur).Error
	if err != nil {
		// 查不到当前状态（设备可能未注册），仍尝试置 offline 并打一条诊断日志。
		logger.Warn("[OFFLINE-DEBUG] device %s set offline (current status lookup failed: %v): reason=%s", deviceID, err, reason)
	} else if cur.Status == "online" {
		logger.Warn("[OFFLINE-DEBUG] device %s ONLINE->OFFLINE: reason=%s", deviceID, reason)
	}
	return s.SetOffline(deviceID)
}

func (s *DeviceService) UpdateLastSeen(deviceID string) error {
	now := time.Now()
	return s.DB.Model(&models.Device{}).
		Where("device_id = ?", deviceID).
		Update("last_seen_at", now).Error
}

func (s *DeviceService) UpdateVersion(deviceID, version string) error {
	return s.DB.Model(&models.Device{}).
		Where("device_id = ?", deviceID).
		Update("version", version).Error
}

// checkBoundReason reports whether a device is bound to a live gateway and,
// if not, a human-readable reason (排查用). 实现见 gateway.DeviceBoundReason。
func (s *DeviceService) MarkStaleDevicesOffline(checkBound func(deviceID string) (bool, string)) (int, error) {
	var devices []models.Device
	if err := s.DB.Where("status = ?", "online").Find(&devices).Error; err != nil {
		return 0, err
	}
	var count int
	for _, d := range devices {
		bound, reason := checkBound(d.DeviceID)
		if !bound {
			// SetOfflineReason 会在 online->offline 转变时打印 reason。
			if err := s.SetOfflineReason(d.DeviceID, "stale-check: "+reason); err == nil {
				count++
			}
		}
	}
	return count, nil
}
