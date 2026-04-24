package services

import (
	"errors"
	"regexp"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrTagNotFound    = errors.New("tag not found")
	ErrTagSlugTaken   = errors.New("tag slug already exists")
	ErrInvalidTagSlug = errors.New("invalid tag slug")
)

const (
	TagClassSystem  = "system"
	TagClassBuiltin = "builtin"
	TagClassCustom  = "custom"
)

var tagSlugPattern = regexp.MustCompile(`^[a-z0-9_-]+$`)

type TagService struct {
	DB *gorm.DB
}

func (s *TagService) ListByClass(tagClass string) ([]models.ItemTagDict, error) {
	if tagClass == "" {
		return nil, nil
	}
	var tags []models.ItemTagDict
	if err := s.DB.Where("tag_class = ?", tagClass).Order("slug ASC").Find(&tags).Error; err != nil {
		return nil, err
	}
	return tags, nil
}

type CreateTagReq struct {
	Slug     string `json:"slug" binding:"required"`
	TagClass string `json:"tagClass" binding:"required"`
}

type UpdateTagReq struct {
	TagClass *string `json:"tagClass"`
}

type ListTagsOptions struct {
	Query    string
	TagClass string
	Page     int
	PageSize int
}

func normalizeTagSlug(slug string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	var b strings.Builder
	b.Grow(len(slug))
	lastDash := false
	for _, r := range slug {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(b.String(), "-")
}

func ValidateTagSlug(slug string) error {
	slug = normalizeTagSlug(slug)
	if slug == "" || !tagSlugPattern.MatchString(slug) {
		return ErrInvalidTagSlug
	}
	return nil
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "UNIQUE constraint") ||
		strings.Contains(msg, "duplicated key not allowed")
}

// EnsureTags ensures tag records exist for the given slugs.
func (s *TagService) EnsureTags(slugs []string, tagClass, createdBy string) ([]models.ItemTagDict, error) {
	if len(slugs) == 0 {
		return nil, nil
	}
	if tagClass != TagClassSystem && tagClass != TagClassBuiltin && tagClass != TagClassCustom {
		tagClass = TagClassCustom
	}

	seen := make(map[string]bool)
	unique := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		slug = normalizeTagSlug(slug)
		if slug == "" || seen[slug] {
			continue
		}
		if err := ValidateTagSlug(slug); err != nil {
			return nil, err
		}
		seen[slug] = true
		unique = append(unique, slug)
	}
	if len(unique) == 0 {
		return nil, nil
	}

	var existing []models.ItemTagDict
	if err := s.DB.Where("slug IN ?", unique).Find(&existing).Error; err != nil {
		return nil, err
	}

	existingMap := make(map[string]models.ItemTagDict, len(existing))
	for _, t := range existing {
		existingMap[t.Slug] = t
	}

	for _, slug := range unique {
		if _, ok := existingMap[slug]; ok {
			continue
		}
		tag := models.ItemTagDict{
			ID:        uuid.NewString(),
			Slug:      slug,
			TagClass:  tagClass,
			CreatedBy: createdBy,
		}
		if err := s.DB.Create(&tag).Error; err != nil {
			if isUniqueConstraintError(err) {
				if err := s.DB.Where("slug = ?", slug).First(&tag).Error; err == nil {
					existingMap[slug] = tag
					continue
				}
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
			itemTag := models.ItemTag{ID: uuid.NewString(), ItemID: itemID, TagID: tagID}
			if err := tx.Create(&itemTag).Error; err != nil {
				if !isUniqueConstraintError(err) {
					return err
				}
			}
		}
		return nil
	})
}

// GetItemTags batch-fetches tags for multiple items.
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

func (s *TagService) GetTagIDsBySlugs(slugs []string) ([]string, error) {
	if len(slugs) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		normalized = append(normalized, normalizeTagSlug(slug))
	}
	var tags []models.ItemTagDict
	if err := s.DB.Select("id").Where("slug IN ?", normalized).Find(&tags).Error; err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(tags))
	for _, t := range tags {
		ids = append(ids, t.ID)
	}
	return ids, nil
}

func (s *TagService) List(opts ListTagsOptions) ([]models.ItemTagDict, int64, error) {
	page := opts.Page
	pageSize := opts.PageSize
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	q := s.DB.Model(&models.ItemTagDict{})
	if opts.TagClass != "" {
		q = q.Where("tag_class = ?", opts.TagClass)
	}
	if query := strings.TrimSpace(opts.Query); query != "" {
		query = strings.ToLower(query)
		q = q.Where("LOWER(slug) LIKE ?", "%"+query+"%")
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var tags []models.ItemTagDict
	err := q.Order("tag_class ASC, slug ASC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&tags).Error
	return tags, total, err
}

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

func (s *TagService) GetBySlug(slug string) (*models.ItemTagDict, error) {
	slug = normalizeTagSlug(slug)
	var tag models.ItemTagDict
	if err := s.DB.Where("slug = ?", slug).First(&tag).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTagNotFound
		}
		return nil, err
	}
	return &tag, nil
}

// ResolveOrCreateForAssignment resolves existing tags by slug first, and only
// creates missing slugs as custom tags. This allows builtin/system tags to be
// referenced directly by slug without downgrading their class.
func (s *TagService) ResolveOrCreateForAssignment(slugs []string, createdBy string) ([]models.ItemTagDict, error) {
	if len(slugs) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool)
	normalized := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		slug = normalizeTagSlug(slug)
		if slug == "" || seen[slug] {
			continue
		}
		if err := ValidateTagSlug(slug); err != nil {
			return nil, err
		}
		seen[slug] = true
		normalized = append(normalized, slug)
	}
	if len(normalized) == 0 {
		return nil, nil
	}

	var existing []models.ItemTagDict
	if err := s.DB.Where("slug IN ?", normalized).Find(&existing).Error; err != nil {
		return nil, err
	}
	existingMap := make(map[string]models.ItemTagDict, len(existing))
	for _, tag := range existing {
		existingMap[tag.Slug] = tag
	}

	missing := make([]string, 0)
	for _, slug := range normalized {
		if _, ok := existingMap[slug]; !ok {
			missing = append(missing, slug)
		}
	}
	if len(missing) > 0 {
		created, err := s.EnsureTags(missing, TagClassCustom, createdBy)
		if err != nil {
			return nil, err
		}
		for _, tag := range created {
			existingMap[tag.Slug] = tag
		}
	}

	result := make([]models.ItemTagDict, 0, len(normalized))
	for _, slug := range normalized {
		if tag, ok := existingMap[slug]; ok {
			result = append(result, tag)
		}
	}
	return result, nil
}

func (s *TagService) Create(req CreateTagReq, createdBy string) (*models.ItemTagDict, error) {
	slug := normalizeTagSlug(req.Slug)
	if err := ValidateTagSlug(slug); err != nil {
		return nil, err
	}
	tagClass := req.TagClass
	if tagClass != TagClassSystem && tagClass != TagClassBuiltin && tagClass != TagClassCustom {
		tagClass = TagClassCustom
	}
	tag := models.ItemTagDict{
		ID:        uuid.NewString(),
		Slug:      slug,
		TagClass:  tagClass,
		CreatedBy: createdBy,
	}
	if err := s.DB.Create(&tag).Error; err != nil {
		if isUniqueConstraintError(err) {
			return nil, ErrTagSlugTaken
		}
		return nil, err
	}
	return &tag, nil
}

func (s *TagService) Update(id string, req UpdateTagReq) (*models.ItemTagDict, error) {
	var tag models.ItemTagDict
	if err := s.DB.First(&tag, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTagNotFound
		}
		return nil, err
	}
	if req.TagClass != nil {
		if *req.TagClass == TagClassSystem || *req.TagClass == TagClassBuiltin || *req.TagClass == TagClassCustom {
			tag.TagClass = *req.TagClass
		}
	}
	if err := s.DB.Save(&tag).Error; err != nil {
		return nil, err
	}
	return &tag, nil
}

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
