package services

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/pathutil"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var (
	ErrWorkspaceNotFound            = errors.New("workspace not found")
	ErrWorkspaceNameExists          = errors.New("workspace name already exists")
	ErrWorkspaceDirectoryRequired   = errors.New("at least one directory is required")
	ErrWorkspaceDirectoryNotFound   = errors.New("workspace directory not found")
	ErrDefaultWorkspaceCannotDelete = errors.New("cannot delete default workspace")
	ErrDeviceNotOwned               = errors.New("device does not belong to current user")
	ErrDirectoryPathDuplicate       = errors.New("directory path must be unique within a workspace")
)

// WorkspaceService 工作空间服务
type WorkspaceService struct {
	DB            *gorm.DB
	DeviceService *DeviceService
}

// WorkspaceWithDeviceStatus 包含设备状态的工作空间响应
type WorkspaceWithDeviceStatus struct {
	models.Workspace
	DeviceStatus   string `json:"deviceStatus"`   // online | offline | busy | ""
	DeviceUniqueID string `json:"deviceUniqueId"` // Device.DeviceID，用于代理路由
}

// CreateWorkspaceRequest 创建工作空间请求
type CreateWorkspaceRequest struct {
	Name        string                   `json:"name" binding:"required,max=100"`
	Description string                   `json:"description" binding:"max=500"`
	DeviceID    string                   `json:"deviceId"`
	Directories []CreateDirectoryRequest `json:"directories" binding:"required,min=1"`
	Settings    map[string]interface{}   `json:"settings"`
}

// CreateDirectoryRequest 创建目录请求
type CreateDirectoryRequest struct {
	Name      string                 `json:"name" binding:"required,max=100"`
	Path      string                 `json:"path" binding:"required,max=500"`
	IsDefault bool                   `json:"isDefault"`
	Settings  map[string]interface{} `json:"settings"`
}

// UpdateWorkspaceRequest 更新工作空间请求
type UpdateWorkspaceRequest struct {
	Name        string                 `json:"name" binding:"omitempty,max=100"`
	Description string                 `json:"description" binding:"omitempty,max=500"`
	DeviceID    string                 `json:"deviceId"`
	Settings    map[string]interface{} `json:"settings"`
	Status      string                 `json:"status" binding:"omitempty,oneof=active inactive archived"`
}

// UpdateDirectoryRequest 更新目录请求
type UpdateDirectoryRequest struct {
	Name      string                 `json:"name" binding:"omitempty,max=100"`
	Path      string                 `json:"path" binding:"omitempty,max=500"`
	IsDefault bool                   `json:"isDefault"`
	Settings  map[string]interface{} `json:"settings"`
}

// ReorderDirectoriesRequest 重新排序目录请求
type ReorderDirectoriesRequest struct {
	DirectoryIDs []string `json:"directoryIds" binding:"required"`
}

// toDatatypesJSON 将 map[string]interface{} 转换为 datatypes.JSON
func toDatatypesJSON(m map[string]interface{}) datatypes.JSON {
	if m == nil {
		return datatypes.JSON([]byte("{}"))
	}
	b, err := json.Marshal(m)
	if err != nil {
		return datatypes.JSON([]byte("{}"))
	}
	return datatypes.JSON(b)
}

// getWorkspaceModel 内部方法：获取工作空间模型（不包含设备状态）
func (s *WorkspaceService) getWorkspaceModel(workspaceID, userID string) (*models.Workspace, error) {
	var workspace models.Workspace
	result := s.DB.Preload("Directories").Where("id = ? AND user_id = ?", workspaceID, userID).First(&workspace)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrWorkspaceNotFound
		}
		return nil, result.Error
	}

	// 按 OrderIndex 排序目录
	var directories []models.WorkspaceDirectory
	if err := s.DB.Where("workspace_id = ?", workspaceID).Order("order_index ASC").Find(&directories).Error; err != nil {
		return nil, err
	}
	workspace.Directories = directories

	return &workspace, nil
}

// CreateWorkspace 创建工作空间
func (s *WorkspaceService) CreateWorkspace(userID string, req CreateWorkspaceRequest) (*WorkspaceWithDeviceStatus, error) {
	// 检查名称是否已存在
	var existing models.Workspace
	result := s.DB.Where("user_id = ? AND name = ?", userID, req.Name).First(&existing)
	if result.Error == nil {
		return nil, ErrWorkspaceNameExists
	}
	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return nil, result.Error
	}

	// 验证设备（如果指定了设备ID）
	if req.DeviceID != "" && s.DeviceService != nil {
		device, err := s.DeviceService.GetDeviceByID(req.DeviceID, userID)
		if err != nil {
			if errors.Is(err, ErrDeviceNotFound) {
				return nil, fmt.Errorf("%w: %s", ErrDeviceNotFound, req.DeviceID)
			}
			return nil, fmt.Errorf("failed to verify device: %w", err)
		}
		if device == nil {
			return nil, fmt.Errorf("device not found: %s", req.DeviceID)
		}
	}

	// 检查目录路径是否重复
	pathMap := make(map[string]bool)
	for i, dir := range req.Directories {
		if pathMap[dir.Path] {
			return nil, fmt.Errorf("duplicate directory path at position %d: %s", i+1, dir.Path)
		}
		pathMap[dir.Path] = true
	}

	// 事务处理
	tx := s.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// 创建工作空间
	workspace := &models.Workspace{
		Name:        req.Name,
		Description: req.Description,
		UserID:      userID,
		DeviceID:    req.DeviceID,
		Status:      "active",
	}

	if req.Settings != nil {
		workspace.Settings = toDatatypesJSON(req.Settings)
	}

	// 如果是用户的第一个工作空间，设为默认
	var count int64
	if err := tx.Model(&models.Workspace{}).Where("user_id = ?", userID).Count(&count).Error; err != nil {
		tx.Rollback()
		return nil, err
	}
	workspace.IsDefault = count == 0

	if err := tx.Create(workspace).Error; err != nil {
		tx.Rollback()
		return nil, err
	}

	// 创建工作目录
	hasDefault := false
	for i, dirReq := range req.Directories {
		directory := &models.WorkspaceDirectory{
			WorkspaceID: workspace.ID,
			Name:        dirReq.Name,
			Path:        pathutil.NormalizeWorkspacePath(dirReq.Path),
			IsDefault:   dirReq.IsDefault,
			OrderIndex:  i,
		}
		if dirReq.Settings != nil {
			directory.Settings = toDatatypesJSON(dirReq.Settings)
		}

		if dirReq.IsDefault {
			hasDefault = true
		}

		if err := tx.Create(directory).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
	}

	// 如果没有设置默认目录，将第一个设为默认
	if !hasDefault && len(req.Directories) > 0 {
		if err := tx.Model(&models.WorkspaceDirectory{}).
			Where("workspace_id = ? AND order_index = 0", workspace.ID).
			Update("is_default", true).Error; err != nil {
			tx.Rollback()
			return nil, err
		}
	}

	if err := tx.Commit().Error; err != nil {
		return nil, err
	}

	// 重新加载完整数据
	return s.GetWorkspace(workspace.ID, userID)
}

// GetWorkspace 获取工作空间详情
func (s *WorkspaceService) GetWorkspace(workspaceID, userID string) (*WorkspaceWithDeviceStatus, error) {
	var workspace models.Workspace
	result := s.DB.Preload("Directories").Where("id = ? AND user_id = ?", workspaceID, userID).First(&workspace)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrWorkspaceNotFound
		}
		return nil, result.Error
	}

	// 按 OrderIndex 排序目录
	var directories []models.WorkspaceDirectory
	if err := s.DB.Where("workspace_id = ?", workspaceID).Order("order_index ASC").Find(&directories).Error; err != nil {
		return nil, err
	}
	workspace.Directories = directories

	// 获取设备状态
	deviceStatus := ""
	deviceUniqueID := ""
	if workspace.DeviceID != "" && s.DeviceService != nil {
		device, err := s.DeviceService.GetDeviceByID(workspace.DeviceID, userID)
		if err == nil {
			deviceStatus = device.Status
			deviceUniqueID = device.DeviceID
		}
	}

	return &WorkspaceWithDeviceStatus{
		Workspace:      workspace,
		DeviceStatus:   deviceStatus,
		DeviceUniqueID: deviceUniqueID,
	}, nil
}

// ListWorkspaces 列出用户的所有工作空间
func (s *WorkspaceService) ListWorkspaces(userID string) ([]WorkspaceWithDeviceStatus, error) {
	var workspaces []models.Workspace
	if err := s.DB.Where("user_id = ?", userID).Order("is_default DESC, created_at DESC").Find(&workspaces).Error; err != nil {
		return nil, err
	}

	result := make([]WorkspaceWithDeviceStatus, 0, len(workspaces))

	// 加载每个工作空间的目录和设备状态
	for i := range workspaces {
		var directories []models.WorkspaceDirectory
		if err := s.DB.Where("workspace_id = ?", workspaces[i].ID).Order("order_index ASC").Find(&directories).Error; err != nil {
			return nil, err
		}
		workspaces[i].Directories = directories

		// 获取设备状态
		deviceStatus := ""
		deviceUniqueID := ""
		if workspaces[i].DeviceID != "" && s.DeviceService != nil {
			device, err := s.DeviceService.GetDeviceByID(workspaces[i].DeviceID, userID)
			if err == nil {
				deviceStatus = device.Status
				deviceUniqueID = device.DeviceID
			}
		}

		result = append(result, WorkspaceWithDeviceStatus{
			Workspace:      workspaces[i],
			DeviceStatus:   deviceStatus,
			DeviceUniqueID: deviceUniqueID,
		})
	}

	return result, nil
}

// UpdateWorkspace 更新工作空间
func (s *WorkspaceService) UpdateWorkspace(workspaceID, userID string, req UpdateWorkspaceRequest) (*WorkspaceWithDeviceStatus, error) {
	workspace, err := s.getWorkspaceModel(workspaceID, userID)
	if err != nil {
		return nil, err
	}

	updates := map[string]interface{}{}

	if req.Name != "" && req.Name != workspace.Name {
		// 检查新名称是否已存在
		var existing models.Workspace
		result := s.DB.Where("user_id = ? AND name = ? AND id != ?", userID, req.Name, workspaceID).First(&existing)
		if result.Error == nil {
			return nil, ErrWorkspaceNameExists
		}
		if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, result.Error
		}
		updates["name"] = req.Name
	}

	if req.Description != "" {
		updates["description"] = req.Description
	}

	if req.DeviceID != "" {
		updates["device_id"] = req.DeviceID
	}

	if req.Status != "" {
		updates["status"] = req.Status
	}

	if req.Settings != nil {
		updates["settings"] = req.Settings
	}

	if len(updates) > 0 {
		if err := s.DB.Model(workspace).Updates(updates).Error; err != nil {
			return nil, err
		}
	}

	return s.GetWorkspace(workspaceID, userID)
}

// DeleteWorkspace 删除工作空间
func (s *WorkspaceService) DeleteWorkspace(workspaceID, userID string) error {
	workspace, err := s.getWorkspaceModel(workspaceID, userID)
	if err != nil {
		return err
	}

	// 如果是默认工作空间，需要先将其他工作空间设为默认
	if workspace.IsDefault {
		// 查找该用户的其他工作空间（按创建时间最早的优先）
		var otherWorkspace models.Workspace
		err := s.DB.Where("user_id = ? AND id != ?", userID, workspaceID).
			Order("created_at ASC").
			Limit(1).
			Find(&otherWorkspace).Error

		if err == nil && otherWorkspace.ID != "" {
			// 找到其他工作空间，将其设为默认
			if err := s.SetDefaultWorkspace(otherWorkspace.ID, userID); err != nil {
				return err
			}
		}
		// 如果没有其他工作空间(result.Error == gorm.ErrRecordNotFound)，直接删除即可
	}

	// 软删除（会级联删除目录）
	return s.DB.Delete(workspace).Error
}

// SetDefaultWorkspace 设置默认工作空间
func (s *WorkspaceService) SetDefaultWorkspace(workspaceID, userID string) error {
	tx := s.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	// 先将用户的所有工作空间设为非默认
	if err := tx.Model(&models.Workspace{}).
		Where("user_id = ?", userID).
		Update("is_default", false).Error; err != nil {
		tx.Rollback()
		return err
	}

	// 将指定工作空间设为默认
	result := tx.Model(&models.Workspace{}).
		Where("id = ? AND user_id = ?", workspaceID, userID).
		Update("is_default", true)

	if result.Error != nil {
		tx.Rollback()
		return result.Error
	}

	if result.RowsAffected == 0 {
		tx.Rollback()
		return ErrWorkspaceNotFound
	}

	return tx.Commit().Error
}

// AddDirectory 添加工作目录
func (s *WorkspaceService) AddDirectory(workspaceID, userID string, req CreateDirectoryRequest) (*models.WorkspaceDirectory, error) {
	// 验证工作空间存在且属于当前用户
	workspace, err := s.getWorkspaceModel(workspaceID, userID)
	if err != nil {
		return nil, err
	}

	// 计算新的 order_index
	maxOrder := 0
	for _, dir := range workspace.Directories {
		if dir.OrderIndex > maxOrder {
			maxOrder = dir.OrderIndex
		}
	}

	directory := &models.WorkspaceDirectory{
		WorkspaceID: workspaceID,
		Name:        req.Name,
		Path:        pathutil.NormalizeWorkspacePath(req.Path),
		IsDefault:   req.IsDefault,
		OrderIndex:  maxOrder + 1,
	}

	if req.Settings != nil {
		directory.Settings = toDatatypesJSON(req.Settings)
	}

	// 如果设置为默认，需要将其他目录设为非默认
	if req.IsDefault {
		if err := s.DB.Model(&models.WorkspaceDirectory{}).
			Where("workspace_id = ?", workspaceID).
			Update("is_default", false).Error; err != nil {
			return nil, err
		}
	}

	if err := s.DB.Create(directory).Error; err != nil {
		return nil, err
	}

	return directory, nil
}

// UpdateDirectory 更新工作目录
func (s *WorkspaceService) UpdateDirectory(workspaceID, directoryID, userID string, req UpdateDirectoryRequest) (*models.WorkspaceDirectory, error) {
	// 验证工作空间存在且属于当前用户
	_, err := s.getWorkspaceModel(workspaceID, userID)
	if err != nil {
		return nil, err
	}

	var directory models.WorkspaceDirectory
	result := s.DB.Where("id = ? AND workspace_id = ?", directoryID, workspaceID).First(&directory)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrWorkspaceDirectoryNotFound
		}
		return nil, result.Error
	}

	updates := map[string]interface{}{}

	if req.Name != "" {
		updates["name"] = req.Name
	}

	if req.Path != "" {
		updates["path"] = pathutil.NormalizeWorkspacePath(req.Path)
	}

	if req.Settings != nil {
		updates["settings"] = req.Settings
	}

	// 处理默认目录变更
	if req.IsDefault != directory.IsDefault {
		if req.IsDefault {
			// 先将其他目录设为非默认
			if err := s.DB.Model(&models.WorkspaceDirectory{}).
				Where("workspace_id = ? AND id != ?", workspaceID, directoryID).
				Update("is_default", false).Error; err != nil {
				return nil, err
			}
		}
		updates["is_default"] = req.IsDefault
	}

	if len(updates) > 0 {
		if err := s.DB.Model(&directory).Updates(updates).Error; err != nil {
			return nil, err
		}
	}

	return &directory, nil
}

// DeleteDirectory 删除工作目录
func (s *WorkspaceService) DeleteDirectory(workspaceID, directoryID, userID string) error {
	// 验证工作空间存在且属于当前用户
	workspace, err := s.getWorkspaceModel(workspaceID, userID)
	if err != nil {
		return err
	}

	// 检查是否至少保留一个目录
	if len(workspace.Directories) <= 1 {
		return fmt.Errorf("%w: workspace must have at least one directory", ErrWorkspaceDirectoryRequired)
	}

	var directory models.WorkspaceDirectory
	result := s.DB.Where("id = ? AND workspace_id = ?", directoryID, workspaceID).First(&directory)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return ErrWorkspaceDirectoryNotFound
		}
		return result.Error
	}

	// 如果删除的是默认目录，需要将第一个剩余目录设为默认
	if directory.IsDefault {
		if err := s.DB.Model(&models.WorkspaceDirectory{}).
			Where("workspace_id = ? AND id != ?", workspaceID, directoryID).
			Order("order_index ASC").
			Limit(1).
			Update("is_default", true).Error; err != nil {
			return err
		}
	}

	return s.DB.Delete(&directory).Error
}

// ReorderDirectories 重新排序目录
func (s *WorkspaceService) ReorderDirectories(workspaceID, userID string, req ReorderDirectoriesRequest) error {
	// 验证工作空间存在且属于当前用户
	_, err := s.getWorkspaceModel(workspaceID, userID)
	if err != nil {
		return err
	}

	tx := s.DB.Begin()
	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
		}
	}()

	for i, dirID := range req.DirectoryIDs {
		result := tx.Model(&models.WorkspaceDirectory{}).
			Where("id = ? AND workspace_id = ?", dirID, workspaceID).
			Update("order_index", i)

		if result.Error != nil {
			tx.Rollback()
			return result.Error
		}
	}

	return tx.Commit().Error
}

// GetDefaultWorkspace 获取用户的默认工作空间
func (s *WorkspaceService) GetDefaultWorkspace(userID string) (*WorkspaceWithDeviceStatus, error) {
	var workspace models.Workspace
	result := s.DB.Preload("Directories").Where("user_id = ? AND is_default = ?", userID, true).First(&workspace)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrWorkspaceNotFound
		}
		return nil, result.Error
	}

	// 按 OrderIndex 排序目录
	var directories []models.WorkspaceDirectory
	if err := s.DB.Where("workspace_id = ?", workspace.ID).Order("order_index ASC").Find(&directories).Error; err != nil {
		return nil, err
	}
	workspace.Directories = directories

	// 获取设备状态
	deviceStatus := ""
	deviceUniqueID := ""
	if workspace.DeviceID != "" && s.DeviceService != nil {
		device, err := s.DeviceService.GetDeviceByID(workspace.DeviceID, userID)
		if err == nil {
			deviceStatus = device.Status
			deviceUniqueID = device.DeviceID
		}
	}

	return &WorkspaceWithDeviceStatus{
		Workspace:      workspace,
		DeviceStatus:   deviceStatus,
		DeviceUniqueID: deviceUniqueID,
	}, nil
}

// GetWorkspaceByDevice 获取设备关联的工作空间
func (s *WorkspaceService) GetWorkspaceByDevice(deviceID, userID string) (*WorkspaceWithDeviceStatus, error) {
	var workspace models.Workspace
	result := s.DB.Preload("Directories").Where("device_id = ? AND user_id = ?", deviceID, userID).First(&workspace)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return nil, ErrWorkspaceNotFound
		}
		return nil, result.Error
	}

	// 按 OrderIndex 排序目录
	var directories []models.WorkspaceDirectory
	if err := s.DB.Where("workspace_id = ?", workspace.ID).Order("order_index ASC").Find(&directories).Error; err != nil {
		return nil, err
	}
	workspace.Directories = directories

	// 获取设备状态
	deviceStatus := ""
	deviceUniqueID := ""
	if workspace.DeviceID != "" && s.DeviceService != nil {
		device, err := s.DeviceService.GetDeviceByID(workspace.DeviceID, userID)
		if err == nil {
			deviceStatus = device.Status
			deviceUniqueID = device.DeviceID
		}
	}

	return &WorkspaceWithDeviceStatus{
		Workspace:      workspace,
		DeviceStatus:   deviceStatus,
		DeviceUniqueID: deviceUniqueID,
	}, nil
}

// ResolveWorkspaceForGateway resolves a workspace to device ID and default directory.
// This is used by the gateway session service to translate workspace IDs into
// device-specific information for proxying requests.
func (s *WorkspaceService) ResolveWorkspaceForGateway(workspaceID, userID string) (deviceID string, directory string, err error) {
	var workspace models.Workspace
	result := s.DB.Where("id = ? AND user_id = ?", workspaceID, userID).First(&workspace)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return "", "", ErrWorkspaceNotFound
		}
		return "", "", result.Error
	}

	if workspace.DeviceID == "" {
		return "", "", fmt.Errorf("workspace has no device bound")
	}

	// Find the default directory
	var defaultDir models.WorkspaceDirectory
	result = s.DB.Where("workspace_id = ? AND is_default = ?", workspaceID, true).First(&defaultDir)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			// If no default directory, use the first one
			result = s.DB.Where("workspace_id = ?", workspaceID).Order("order_index ASC").Limit(1).First(&defaultDir)
			if result.Error != nil {
				return "", "", fmt.Errorf("workspace has no directories")
			}
		} else {
			return "", "", result.Error
		}
	}

	return workspace.DeviceID, defaultDir.Path, nil
}
