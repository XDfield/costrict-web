package services

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var (
	ErrCategoryNotFound  = errors.New("category not found")
	ErrCategorySlugTaken = errors.New("category slug already exists")
)

type CategoryService struct {
	DB *gorm.DB
}

type CreateCategoryReq struct {
	Slug         string         `json:"slug" binding:"required"`
	Icon         string         `json:"icon"`
	SortOrder    int            `json:"sortOrder"`
	Names        map[string]string `json:"names" binding:"required"`
	Descriptions map[string]string `json:"descriptions"`
}

type UpdateCategoryReq struct {
	Icon         *string           `json:"icon"`
	SortOrder    *int              `json:"sortOrder"`
	Names        map[string]string `json:"names"`
	Descriptions map[string]string `json:"descriptions"`
}

// EnsureCategory ensures a category record exists for the given slug.
// If the slug is empty, it returns nil. If it doesn't exist, it creates
// a minimal record with the slug as the default English name.
func (s *CategoryService) EnsureCategory(slug, createdBy string) (*models.ItemCategory, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, nil
	}

	var cat models.ItemCategory
	err := s.DB.Where("slug = ?", slug).First(&cat).Error
	if err == nil {
		return &cat, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	names, _ := json.Marshal(map[string]string{"en": slug})
	cat = models.ItemCategory{
		Slug:      slug,
		Names:     datatypes.JSON(names),
		CreatedBy: createdBy,
	}
	if err := s.DB.Create(&cat).Error; err != nil {
		// Handle race condition: another goroutine may have created it.
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "UNIQUE constraint") {
			s.DB.Where("slug = ?", slug).First(&cat)
			return &cat, nil
		}
		return nil, err
	}
	return &cat, nil
}

func (s *CategoryService) List() ([]models.ItemCategory, error) {
	var categories []models.ItemCategory
	err := s.DB.Order("sort_order ASC, slug ASC").Find(&categories).Error
	return categories, err
}

func (s *CategoryService) Get(id string) (*models.ItemCategory, error) {
	var cat models.ItemCategory
	if err := s.DB.First(&cat, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrCategoryNotFound
		}
		return nil, err
	}
	return &cat, nil
}

func (s *CategoryService) Create(req CreateCategoryReq, createdBy string) (*models.ItemCategory, error) {
	names, _ := json.Marshal(req.Names)
	descs, _ := json.Marshal(req.Descriptions)
	if descs == nil {
		descs = []byte("{}")
	}

	cat := models.ItemCategory{
		Slug:         req.Slug,
		Icon:         req.Icon,
		SortOrder:    req.SortOrder,
		Names:        datatypes.JSON(names),
		Descriptions: datatypes.JSON(descs),
		CreatedBy:    createdBy,
	}
	if err := s.DB.Create(&cat).Error; err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "UNIQUE constraint") {
			return nil, ErrCategorySlugTaken
		}
		return nil, err
	}
	return &cat, nil
}

func (s *CategoryService) Update(id string, req UpdateCategoryReq) (*models.ItemCategory, error) {
	var cat models.ItemCategory
	if err := s.DB.First(&cat, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrCategoryNotFound
		}
		return nil, err
	}

	if req.Icon != nil {
		cat.Icon = *req.Icon
	}
	if req.SortOrder != nil {
		cat.SortOrder = *req.SortOrder
	}
	if req.Names != nil {
		names, _ := json.Marshal(req.Names)
		cat.Names = datatypes.JSON(names)
	}
	if req.Descriptions != nil {
		descs, _ := json.Marshal(req.Descriptions)
		cat.Descriptions = datatypes.JSON(descs)
	}

	if err := s.DB.Save(&cat).Error; err != nil {
		return nil, err
	}
	return &cat, nil
}

func (s *CategoryService) Delete(id string) error {
	result := s.DB.Delete(&models.ItemCategory{}, "id = ?", id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrCategoryNotFound
	}
	return nil
}
