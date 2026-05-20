package memory

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/storage"
	"gorm.io/gorm"
)

// CreateMemoryRequest 创建/上报记忆请求
type CreateMemoryRequest struct {
	Name        string `json:"name" binding:"required"`
	Slug        string `json:"slug" binding:"required"`
	ProjectPath string `json:"projectPath" binding:"required"`
	WorkDir     string `json:"workDir"`
	Type        string `json:"type" binding:"required,oneof=user feedback project reference"`
	Description string `json:"description"`
	Content     string `json:"content" binding:"required"`
}

// UpdateMemoryRequest 更新记忆请求
type UpdateMemoryRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content" binding:"required"`
	BumpVersion bool   `json:"bumpVersion"`
}

// ListMemoriesRequest 查询记忆列表请求
type ListMemoriesRequest struct {
	ProjectPath string `form:"projectPath"`
	WorkDir     string `form:"workDir"`
	Type        string `form:"type"`
	Keyword     string `form:"keyword"`
}

type Service struct {
	DB      *gorm.DB
	Storage storage.Backend
}

func NewService(db *gorm.DB, st storage.Backend) *Service {
	return &Service{DB: db, Storage: st}
}

func (s *Service) buildStorageKey(userID, memoryID string, version int) string {
	return fmt.Sprintf("memory/%s/%s/v%d.md", userID, memoryID, version)
}

func md5Hash(content string) string {
	h := md5.New()
	_, _ = h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

// CreateOrUpdateMemory 创建或更新记忆
// 如果 (userID, projectPath, slug) 已存在则更新，否则创建
func (s *Service) CreateOrUpdateMemory(ctx context.Context, userID string, req *CreateMemoryRequest) (*models.MemoryFile, error) {
	var existing models.MemoryFile
	err := s.DB.Where("user_id = ? AND project_path = ? AND slug = ? AND deleted_at IS NULL", userID, req.ProjectPath, req.Slug).First(&existing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	if err == nil {
		// 已存在，执行更新（默认创建新版本）
		return s.UpdateMemory(ctx, userID, existing.ID, &UpdateMemoryRequest{
			Name:        req.Name,
			Description: req.Description,
			Content:     req.Content,
			BumpVersion: true,
		})
	}

	// 创建新记忆
	memory := &models.MemoryFile{
		UserID:         userID,
		ProjectPath:    req.ProjectPath,
		WorkDir:        req.WorkDir,
		Name:           req.Name,
		Slug:           req.Slug,
		Type:           req.Type,
		Description:    req.Description,
		CurrentVersion: 1,
	}

	if err := s.DB.Create(memory).Error; err != nil {
		return nil, err
	}

	// 存储文件内容
	contentMD5 := md5Hash(req.Content)
	storageKey := s.buildStorageKey(userID, memory.ID, 1)
	if err := s.Storage.Put(ctx, storageKey, strings.NewReader(req.Content), int64(len(req.Content))); err != nil {
		return nil, err
	}

	// 创建版本记录
	version := &models.MemoryVersion{
		MemoryFileID: memory.ID,
		Version:      1,
		ContentMD5:   contentMD5,
		StorageKey:   storageKey,
	}
	if err := s.DB.Create(version).Error; err != nil {
		return nil, err
	}

	return memory, nil
}

// UpdateMemory 更新记忆
func (s *Service) UpdateMemory(ctx context.Context, userID, memoryID string, req *UpdateMemoryRequest) (*models.MemoryFile, error) {
	var memory models.MemoryFile
	if err := s.DB.Where("id = ? AND user_id = ? AND deleted_at IS NULL", memoryID, userID).First(&memory).Error; err != nil {
		return nil, err
	}

	updates := map[string]interface{}{}
	if req.Name != "" {
		updates["name"] = req.Name
		memory.Name = req.Name
	}
	if req.Description != "" {
		updates["description"] = req.Description
		memory.Description = req.Description
	}

	contentMD5 := md5Hash(req.Content)

	if req.BumpVersion {
		// 创建新版本
		memory.CurrentVersion++
		updates["current_version"] = memory.CurrentVersion

		storageKey := s.buildStorageKey(userID, memory.ID, memory.CurrentVersion)
		if err := s.Storage.Put(ctx, storageKey, strings.NewReader(req.Content), int64(len(req.Content))); err != nil {
			return nil, err
		}

		version := &models.MemoryVersion{
			MemoryFileID: memory.ID,
			Version:      memory.CurrentVersion,
			ContentMD5:   contentMD5,
			StorageKey:   storageKey,
		}
		if err := s.DB.Create(version).Error; err != nil {
			return nil, err
		}
	} else {
		// 覆盖当前版本
		var currentVersion models.MemoryVersion
		if err := s.DB.Where("memory_file_id = ? AND version = ?", memory.ID, memory.CurrentVersion).First(&currentVersion).Error; err != nil {
			return nil, err
		}

		storageKey := s.buildStorageKey(userID, memory.ID, memory.CurrentVersion)
		if err := s.Storage.Put(ctx, storageKey, strings.NewReader(req.Content), int64(len(req.Content))); err != nil {
			return nil, err
		}

		currentVersion.ContentMD5 = contentMD5
		currentVersion.StorageKey = storageKey
		if err := s.DB.Save(&currentVersion).Error; err != nil {
			return nil, err
		}
	}

	updates["updated_at"] = time.Now()
	if err := s.DB.Model(&memory).Updates(updates).Error; err != nil {
		return nil, err
	}

	return &memory, nil
}

// ListMemories 查询记忆列表
func (s *Service) ListMemories(userID string, req *ListMemoriesRequest) ([]models.MemoryFile, error) {
	var memories []models.MemoryFile
	db := s.DB.Where("user_id = ? AND deleted_at IS NULL", userID)

	if req.ProjectPath != "" {
		db = db.Where("project_path = ?", req.ProjectPath)
	}
	if req.WorkDir != "" {
		db = db.Where("work_dir = ?", req.WorkDir)
	}
	if req.Type != "" {
		db = db.Where("type = ?", req.Type)
	}
	if req.Keyword != "" {
		keyword := "%" + req.Keyword + "%"
		db = db.Where("name ILIKE ? OR description ILIKE ?", keyword, keyword)
	}

	if err := db.Order("updated_at DESC").Find(&memories).Error; err != nil {
		return nil, err
	}
	return memories, nil
}

// GetMemory 获取记忆详情
func (s *Service) GetMemory(userID, memoryID string) (*models.MemoryFile, error) {
	var memory models.MemoryFile
	if err := s.DB.Where("id = ? AND user_id = ? AND deleted_at IS NULL", memoryID, userID).First(&memory).Error; err != nil {
		return nil, err
	}
	return &memory, nil
}

// GetMemoryContent 获取记忆当前版本内容
func (s *Service) GetMemoryContent(ctx context.Context, userID, memoryID string) (string, error) {
	memory, err := s.GetMemory(userID, memoryID)
	if err != nil {
		return "", err
	}

	var version models.MemoryVersion
	if err := s.DB.Where("memory_file_id = ? AND version = ?", memory.ID, memory.CurrentVersion).First(&version).Error; err != nil {
		return "", err
	}

	reader, _, err := s.Storage.Get(ctx, version.StorageKey)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

// ListVersions 获取版本列表
func (s *Service) ListVersions(userID, memoryID string) ([]models.MemoryVersion, error) {
	memory, err := s.GetMemory(userID, memoryID)
	if err != nil {
		return nil, err
	}

	var versions []models.MemoryVersion
	if err := s.DB.Where("memory_file_id = ?", memory.ID).Order("version DESC").Find(&versions).Error; err != nil {
		return nil, err
	}
	return versions, nil
}

// GetVersionContent 获取指定版本内容
func (s *Service) GetVersionContent(ctx context.Context, userID, memoryID string, versionNum int) (string, error) {
	memory, err := s.GetMemory(userID, memoryID)
	if err != nil {
		return "", err
	}

	var version models.MemoryVersion
	if err := s.DB.Where("memory_file_id = ? AND version = ?", memory.ID, versionNum).First(&version).Error; err != nil {
		return "", err
	}

	reader, _, err := s.Storage.Get(ctx, version.StorageKey)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	content, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

// DeleteMemory 删除记忆（软删除）
func (s *Service) DeleteMemory(userID, memoryID string) error {
	return s.DB.Where("id = ? AND user_id = ?", memoryID, userID).Delete(&models.MemoryFile{}).Error
}
