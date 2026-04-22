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
	ErrTagNotFound  = errors.New("tag not found")
	ErrTagSlugTaken = errors.New("tag slug already exists")
)

const (
	TagClassSystem    = "system"
	TagClassFunctional = "functional"
	TagClassCustom    = "custom"
)

type TagService struct {
	DB *gorm.DB
}

type CreateTagReq struct {
	Slug         string            `json:"slug" binding:"required"`
	TagClass     string            `json:"tagClass" binding:"required"`
	Names        map[string]string `json:"names" binding:"required"`
	Descriptions map[string]string `json:"descriptions"`
}

type UpdateTagReq struct {
	TagClass     *string           `json:"tagClass"`
	Names        map[string]string `json:"names"`
	Descriptions map[string]string `json:"descriptions"`
}

// EnsureTags ensures tag records exist for the given slugs.
// Returns the resolved tag records. Missing tags are created with the given class.
func (s *TagService) EnsureTags(slugs []string, tagClass, createdBy string) ([]models.ItemTagDict, error) {
	if len(slugs) == 0 {
		return nil, nil
	}

	// Deduplicate
	seen := make(map[string]bool)
	unique := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		slug = strings.TrimSpace(slug)
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		unique = append(unique, slug)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	// Fetch existing
	var existing []models.ItemTagDict
	if err := s.DB.Where("slug IN ?", unique).Find(&existing).Error; err != nil {
		return nil, err
	}

	existingMap := make(map[string]models.ItemTagDict, len(existing))
	for _, t := range existing {
		existingMap[t.Slug] = t
	}

	// Create missing
	for _, slug := range unique {
		if _, ok := existingMap[slug]; ok {
			continue
		}
		names, _ := json.Marshal(map[string]string{"en": slug})
		tag := models.ItemTagDict{
			Slug:      slug,
			TagClass:  tagClass,
			Names:     datatypes.JSON(names),
			CreatedBy: createdBy,
		}
		if err := s.DB.Create(&tag).Error; err != nil {
			if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "UNIQUE constraint") {
				s.DB.Where("slug = ?", slug).First(&tag)
				existingMap[slug] = tag
				continue
			}
			return nil, err
		}
		existingMap[slug] = tag
	}

	result := make([]models.ItemTagDict, 0, len(unique))
	for _, slug := range unique {
		if t, ok := existingMap[slug]; ok {
			result = append(result, t)
		}
	}
	return result, nil
}

// SetItemTags replaces all tags on an item within a transaction.
func (s *TagService) SetItemTags(itemID string, tagIDs []string) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("item_id = ?", itemID).Delete(&models.ItemTag{}).Error; err != nil {
			return err
		}
		for _, tagID := range tagIDs {
			itemTag := models.ItemTag{ItemID: itemID, TagID: tagID}
			if err := tx.Create(&itemTag).Error; err != nil {
				if !strings.Contains(err.Error(), "duplicate key") && !strings.Contains(err.Error(), "UNIQUE constraint") {
					return err
				}
			}
		}
		return nil
	})
}

// GetItemTags batch-fetches tags for multiple items.
// Returns a map of itemID -> []ItemTagDict.
func (s *TagService) GetItemTags(itemIDs []string) (map[string][]models.ItemTagDict, error) {
	if len(itemIDs) == 0 {
		return nil, nil
	}

	var itemTags []models.ItemTag
	if err := s.DB.Where("item_id IN ?", itemIDs).Find(&itemTags).Error; err != nil {
		return nil, err
	}
	if len(itemTags) == 0 {
		return nil, nil
	}

	tagIDs := make([]string, 0, len(itemTags))
	for _, it := range itemTags {
		tagIDs = append(tagIDs, it.TagID)
	}

	var tags []models.ItemTagDict
	if err := s.DB.Where("id IN ?", tagIDs).Find(&tags).Error; err != nil {
		return nil, err
	}

	tagMap := make(map[string]models.ItemTagDict, len(tags))
	for _, t := range tags {
		tagMap[t.ID] = t
	}

	result := make(map[string][]models.ItemTagDict)
	for _, it := range itemTags {
		if t, ok := tagMap[it.TagID]; ok {
			result[it.ItemID] = append(result[it.ItemID], t)
		}
	}
	return result, nil
}

// GetTagIDsBySlugs resolves tag slugs to IDs.
func (s *TagService) GetTagIDsBySlugs(slugs []string) ([]string, error) {
	if len(slugs) == 0 {
		return nil, nil
	}
	var tags []models.ItemTagDict
	if err := s.DB.Select("id").Where("slug IN ?", slugs).Find(&tags).Error; err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(tags))
	for _, t := range tags {
		ids = append(ids, t.ID)
	}
	return ids, nil
}

// List returns all tags, optionally filtered by tagClass.
func (s *TagService) List(tagClass string) ([]models.ItemTagDict, error) {
	var tags []models.ItemTagDict
	q := s.DB.Order("tag_class ASC, slug ASC")
	if tagClass != "" {
		q = q.Where("tag_class = ?", tagClass)
	}
	err := q.Find(&tags).Error
	return tags, err
}

// Get returns a single tag by ID.
func (s *TagService) Get(id string) (*models.ItemTagDict, error) {
	var tag models.ItemTagDict
	if err := s.DB.First(&tag, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTagNotFound
		}
		return nil, err
	}
	return &tag, nil
}

// GetBySlug returns a single tag by slug.
func (s *TagService) GetBySlug(slug string) (*models.ItemTagDict, error) {
	var tag models.ItemTagDict
	if err := s.DB.Where("slug = ?", slug).First(&tag).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTagNotFound
		}
		return nil, err
	}
	return &tag, nil
}

// Create creates a new tag.
func (s *TagService) Create(req CreateTagReq, createdBy string) (*models.ItemTagDict, error) {
	names, _ := json.Marshal(req.Names)
	descs, _ := json.Marshal(req.Descriptions)
	if descs == nil {
		descs = []byte("{}")
	}

	tag := models.ItemTagDict{
		Slug:         req.Slug,
		TagClass:     req.TagClass,
		Names:        datatypes.JSON(names),
		Descriptions: datatypes.JSON(descs),
		CreatedBy:    createdBy,
	}
	if err := s.DB.Create(&tag).Error; err != nil {
		if strings.Contains(err.Error(), "duplicate key") || strings.Contains(err.Error(), "UNIQUE constraint") {
			return nil, ErrTagSlugTaken
		}
		return nil, err
	}
	return &tag, nil
}

// Update updates an existing tag.
func (s *TagService) Update(id string, req UpdateTagReq) (*models.ItemTagDict, error) {
	var tag models.ItemTagDict
	if err := s.DB.First(&tag, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTagNotFound
		}
		return nil, err
	}

	if req.TagClass != nil {
		tag.TagClass = *req.TagClass
	}
	if req.Names != nil {
		names, _ := json.Marshal(req.Names)
		tag.Names = datatypes.JSON(names)
	}
	if req.Descriptions != nil {
		descs, _ := json.Marshal(req.Descriptions)
		tag.Descriptions = datatypes.JSON(descs)
	}

	if err := s.DB.Save(&tag).Error; err != nil {
		return nil, err
	}
	return &tag, nil
}

// Delete removes a tag and all its item associations.
func (s *TagService) Delete(id string) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("tag_id = ?", id).Delete(&models.ItemTag{}).Error; err != nil {
			return err
		}
		result := tx.Delete(&models.ItemTagDict{}, "id = ?", id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrTagNotFound
		}
		return nil
	})
}
