package enterprise

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const (
	// MaxLogoBytes caps the stored logo (base64 data URI) length. The Logo column
	// is TEXT (~1GB) with no body-size middleware, so without this an admin could
	// write an enormous logo that every GET /enterprise-customers then amplifies to
	// all users. 512KB of base64 (~384KB raw image) is ample for a brand logo.
	MaxLogoBytes = 512 * 1024
	// MaxNameBytes caps the display name length (the Name column is unbounded).
	MaxNameBytes = 256
	// logoDataURIPrefix is the required prefix for the base64 image data URI.
	logoDataURIPrefix = "data:image/"
)

var (
	// ErrEnterpriseCustomerNotFound is returned when an update/delete targets a
	// non-existent (or already soft-deleted) enterprise customer.
	ErrEnterpriseCustomerNotFound = errors.New("enterprise customer not found")
	// ErrInvalidEnterpriseCustomer is returned for empty name/logo on create.
	ErrInvalidEnterpriseCustomer = errors.New("invalid enterprise customer")
	// ErrLogoTooLarge is returned when the logo exceeds MaxLogoBytes or is not an
	// image data URI.
	ErrLogoTooLarge = errors.New("logo too large or not an image data uri")
	// ErrNameTooLong is returned when the name exceeds MaxNameBytes.
	ErrNameTooLong = errors.New("name too long")
	// ErrLogoInvalid is returned when the logo is not a valid base64 image data
	// URI (missing the ";base64," marker or carrying an undecodable payload).
	ErrLogoInvalid = errors.New("logo is not a valid base64 image data uri")
)

// validateCustomerInput enforces the shared write-side rules for Create/Update:
// non-empty name/logo, name within MaxNameBytes, and logo within MaxLogoBytes and
// shaped as an image data URI. It does NOT touch the List/GET path — the stored
// logo is still returned in full for frontend rendering.
func validateCustomerInput(name, logo string) error {
	if name == "" || logo == "" {
		return ErrInvalidEnterpriseCustomer
	}
	if len(name) > MaxNameBytes {
		return ErrNameTooLong
	}
	if len(logo) > MaxLogoBytes || !strings.HasPrefix(logo, logoDataURIPrefix) {
		return ErrLogoTooLarge
	}
	// The logo must be a base64-encoded data URI: locate the ";base64," marker and
	// verify the payload actually decodes. Run this after the length/prefix checks
	// so an oversized logo still fails with ErrLogoTooLarge.
	idx := strings.Index(logo, ";base64,")
	if idx < 0 {
		return ErrLogoInvalid
	}
	payload := logo[idx+len(";base64,"):]
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return ErrLogoInvalid
	}
	return nil
}

type Service struct {
	db *gorm.DB
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// List returns all non-soft-deleted enterprise customers (gorm's DeletedAt
// scope filters deleted rows automatically).
func (s *Service) List() ([]models.EnterpriseCustomer, error) {
	var customers []models.EnterpriseCustomer
	if err := s.db.Order("created_at ASC").Find(&customers).Error; err != nil {
		return nil, err
	}
	return customers, nil
}

// Create inserts a new enterprise customer. ids are subject_id strings; name and
// logo (base64 data URI) are required.
func (s *Service) Create(name, logo string, ids []string, operatorID string) (*models.EnterpriseCustomer, error) {
	if err := validateCustomerInput(name, logo); err != nil {
		return nil, err
	}
	customer := models.EnterpriseCustomer{
		Name:       name,
		Logo:       logo,
		AccountIDs: marshalIDs(ids),
	}
	if operatorID != "" {
		customer.CreatedBy = &operatorID
	}
	if err := s.db.Create(&customer).Error; err != nil {
		return nil, err
	}
	return &customer, nil
}

// Update mutates name/logo/account_ids of an existing customer. Empty name/logo
// are rejected (a full PUT replaces all three fields).
func (s *Service) Update(id, name, logo string, ids []string) (*models.EnterpriseCustomer, error) {
	if err := validateCustomerInput(name, logo); err != nil {
		return nil, err
	}

	var customer models.EnterpriseCustomer
	if err := s.db.Where("id = ?", id).First(&customer).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrEnterpriseCustomerNotFound
		}
		return nil, err
	}

	customer.Name = name
	customer.Logo = logo
	customer.AccountIDs = marshalIDs(ids)
	if err := s.db.Save(&customer).Error; err != nil {
		return nil, err
	}
	return &customer, nil
}

// Delete soft-deletes a customer by id.
func (s *Service) Delete(id string) error {
	result := s.db.Where("id = ?", id).Delete(&models.EnterpriseCustomer{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrEnterpriseCustomerNotFound
	}
	return nil
}

// decodeIDs unmarshals the account_ids jsonb column into a string slice, falling
// back to an empty slice on null/invalid payloads.
func decodeIDs(raw datatypes.JSON) []string {
	ids := []string{}
	if len(raw) == 0 {
		return ids
	}
	if err := json.Unmarshal(raw, &ids); err != nil {
		return []string{}
	}
	return ids
}

func marshalIDs(ids []string) datatypes.JSON {
	if ids == nil {
		ids = []string{}
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return datatypes.JSON([]byte("[]"))
	}
	return datatypes.JSON(b)
}
