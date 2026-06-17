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
	// URI (missing the ";base64," marker, an unsupported MIME subtype, or carrying
	// an undecodable payload).
	ErrLogoInvalid = errors.New("logo is not a valid base64 image data uri")

	// allowedLogoMIME is the raster-image MIME allowlist for logos. SVG and other
	// types are rejected (image/svg+xml can carry scripts), mirroring the frontend.
	allowedLogoMIME = map[string]bool{
		"image/png":  true,
		"image/jpeg": true,
		"image/gif":  true,
		"image/webp": true,
	}
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
	// Constrain the MIME subtype to a raster-image allowlist. This explicitly
	// rejects image/svg+xml (which can carry scripts) and any other exotic type,
	// matching the frontend's tightened check. The mime sits between "data:" and
	// ";base64," (e.g. "data:image/png;base64,...").
	mime := logo[len("data:"):idx]
	if !allowedLogoMIME[mime] {
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

// Create inserts a new enterprise customer. ids are Casdoor universal_id strings
// (the stable identity anchor); name and logo (base64 data URI) are required. The
// stored jsonb format is unchanged — only the semantics moved from subject_id to
// universal_id (see ResolveSubjectIDs / ResolveMembers for the lookup side).
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

// Update mutates name/logo/account_ids of an existing customer. ids are Casdoor
// universal_id strings (same semantics as Create). Empty name/logo are rejected
// (a full PUT replaces all three fields).
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

// Member is a resolved enterprise-customer account: a stored Casdoor universal_id
// joined against the local users table. SubjectID/Username/DisplayName/AvatarURL
// are best-effort — they are empty when the universal_id has no matching local
// user yet (e.g. the person was configured but has never logged in). This lets the
// admin UI show "who is configured" even before those users exist locally.
type Member struct {
	UniversalID string `json:"universalId"`
	SubjectID   string `json:"subjectId"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	AvatarURL   string `json:"avatarUrl"`
}

// ResolveMembersBatch resolves a set of Casdoor universal_id values to their
// local users in a SINGLE query and returns a map keyed by universal_id. This is
// the shared core used both by ResolveMembers (single customer) and by the list
// handlers (which pass the union of every customer's account_ids so the whole
// list resolves in one IN query — O(1) DB round-trips instead of N+1 per row).
//
// Determinism: when two user rows share the same casdoor_universal_id (the column
// is a plain, non-unique index), the lowest-id row wins. We ORDER BY id ASC and
// only record the FIRST row seen per universal_id, so the resolved Member is
// stable across calls regardless of physical scan order. nil/empty input yields
// an empty map. Map values are filled with subject_id / username / display name /
// avatar; a missing universal_id is simply absent from the map (caller decides
// how to represent "not yet a local user").
func (s *Service) ResolveMembersBatch(universalIDs []string) map[string]Member {
	byUniversalID := make(map[string]Member, len(universalIDs))
	if len(universalIDs) == 0 {
		return byUniversalID
	}

	// Deduplicate inputs so the IN list (and the map capacity) stays tight even if
	// the same universal_id appears across several customers.
	seenInput := make(map[string]struct{}, len(universalIDs))
	uniqueIDs := make([]string, 0, len(universalIDs))
	for _, uid := range universalIDs {
		if uid == "" {
			continue
		}
		if _, ok := seenInput[uid]; ok {
			continue
		}
		seenInput[uid] = struct{}{}
		uniqueIDs = append(uniqueIDs, uid)
	}
	if len(uniqueIDs) == 0 {
		return byUniversalID
	}

	var users []models.User
	// ORDER BY id ASC makes the tiebreak below deterministic (lowest id wins).
	if err := s.db.Where("casdoor_universal_id IN ?", uniqueIDs).Order("id ASC").Find(&users).Error; err != nil {
		// On query error degrade gracefully: return an empty map so callers emit
		// unresolved Members rather than dropping data.
		return byUniversalID
	}

	for _, u := range users {
		if u.CasdoorUniversalID == nil || *u.CasdoorUniversalID == "" {
			continue
		}
		key := *u.CasdoorUniversalID
		// Deterministic tiebreak: keep the first (lowest-id) row, skip later
		// duplicates that share the same non-unique universal_id.
		if _, exists := byUniversalID[key]; exists {
			continue
		}
		m := Member{UniversalID: key, SubjectID: u.SubjectID, Username: u.Username}
		if u.DisplayName != nil {
			m.DisplayName = *u.DisplayName
		}
		if u.AvatarURL != nil {
			m.AvatarURL = *u.AvatarURL
		}
		byUniversalID[key] = m
	}
	return byUniversalID
}

// assembleMembers builds the ordered Member slice for one customer from a shared
// byUniversalID map (the output of ResolveMembersBatch). Order is stable (input
// order preserved) and EVERY input universal_id yields one Member — unresolved
// ones keep an empty SubjectID (meaning "not yet a local user").
func assembleMembers(universalIDs []string, byUniversalID map[string]Member) []Member {
	out := make([]Member, 0, len(universalIDs))
	for _, uid := range universalIDs {
		if m, ok := byUniversalID[uid]; ok {
			m.UniversalID = uid
			out = append(out, m)
			continue
		}
		out = append(out, Member{UniversalID: uid})
	}
	return out
}

// subjectIDsFrom maps one customer's universal_id list to its resolved subject_id
// list using a shared byUniversalID map (the output of ResolveMembersBatch),
// dropping any universal_id with no matching local user. Feeds the PUBLIC store
// endpoint; preserves stored order.
func subjectIDsFrom(universalIDs []string, byUniversalID map[string]Member) []string {
	out := make([]string, 0, len(universalIDs))
	for _, uid := range universalIDs {
		if m, ok := byUniversalID[uid]; ok && m.SubjectID != "" {
			out = append(out, m.SubjectID)
		}
	}
	return out
}

// ResolveMembers resolves each universal_id to its local user (subject_id /
// username / display name / avatar) for a SINGLE customer. Order is stable (input
// order preserved) and EVERY input universal_id yields one Member — unresolved
// ones keep an empty SubjectID (meaning "not yet a local user"). Runs one DB
// query via ResolveMembersBatch. nil-safe on empty input.
func (s *Service) ResolveMembers(universalIDs []string) []Member {
	if len(universalIDs) == 0 {
		return []Member{}
	}
	return assembleMembers(universalIDs, s.ResolveMembersBatch(universalIDs))
}

// ResolveSubjectIDs maps the stored universal_id list to the subject_id list,
// dropping any universal_id that has no matching local user. This feeds the
// PUBLIC store endpoint: the frontend keeps matching on item.created_by
// (subject_id), so universal_id never leaks to non-admin callers. nil-safe.
func (s *Service) ResolveSubjectIDs(universalIDs []string) []string {
	out := make([]string, 0, len(universalIDs))
	for _, m := range s.ResolveMembers(universalIDs) {
		if m.SubjectID != "" {
			out = append(out, m.SubjectID)
		}
	}
	return out
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
