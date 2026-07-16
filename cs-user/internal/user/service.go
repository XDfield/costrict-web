// Package user implements cs-user's read-side data access — the methods
// consumed by the read-through RPC client that costrict-web installs in P0-7
// to delegate user lookups to this service.
//
// This is a strict subset of server/internal/user/service.go's UserService.
// Write paths (bind / unbind / transfer / GetOrCreate) deliberately stay
// out of scope for P0-3: they depend on JWT-claims plumbing that lands in
// Phase A. Adding them here would force a half-finished dependency surface
// before the JWT side is designed.
package user

import (
	"errors"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

// Default cap on SearchUsers when the caller doesn't pass one. Keeps a
// runaway query from yanking the whole table into memory if the RPC client
// forgets to clamp.
const defaultSearchLimit = 50

// Service exposes the read-side operations costrict-web needs.
//
// Constructed once at boot (main.go wires it to the handlers); tests inject
// a gorm-backed sqlite DB so the same code path runs against real SQL.
type Service struct {
	db *gorm.DB
}

// NewService returns a Service bound to the supplied gorm pool. Callers own
// the pool's lifecycle (typically cs-user/internal/storage.Pool.Gorm).
func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// GetUserByID returns the user with the given subject_id, or
// gorm.ErrRecordNotFound when no such row exists. The error is wrapped by
// the caller-facing layer so handlers can map it to HTTP 404 without
// importing gorm.
func (s *Service) GetUserByID(subjectID string) (*models.User, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if subjectID == "" {
		return nil, ErrEmptySubjectID
	}

	var u models.User
	if err := s.db.Where("subject_id = ?", subjectID).Take(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUsersByIDs returns a subject_id → User map for the given IDs. Missing
// rows are silently omitted (callers compare len(returned) vs len(input) to
// detect partial misses). Empty input returns an empty map without touching
// the DB — saves a round-trip on degenerate RPC calls.
func (s *Service) GetUsersByIDs(subjectIDs []string) (map[string]*models.User, error) {
	out := make(map[string]*models.User)
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if len(subjectIDs) == 0 {
		return out, nil
	}

	var users []*models.User
	if err := s.db.Where("subject_id IN ?", subjectIDs).Find(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.SubjectID] = u
	}
	return out, nil
}

// SearchUsers returns active users whose username / display_name / email
// match the keyword (LIKE %keyword%, case-insensitive on Postgres via ILIKE
// is intentionally NOT used — server's existing search uses plain LIKE, so
// we match its behaviour to keep result sets comparable during cutover).
//
// limit ≤ 0 falls back to defaultSearchLimit.
func (s *Service) SearchUsers(keyword string, limit int) ([]*models.User, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	query := s.db.Where("is_active = ?", true)
	if keyword != "" {
		pattern := "%" + keyword + "%"
		// Parens are load-bearing: without them SQL's AND binds tighter than
		// OR and the keyword filter leaks inactive rows back in.
		query = query.Where(
			"(username LIKE ? OR display_name LIKE ? OR email LIKE ?)",
			pattern, pattern, pattern,
		)
	}

	var users []*models.User
	err := query.Limit(limit).Find(&users).Error
	return users, err
}

// ListIdentities returns every auth identity bound to the user, ordered so
// the primary identity surfaces first (callers building "linked accounts"
// UI render it at the top).
func (s *Service) ListIdentities(userSubjectID string) ([]*models.UserAuthIdentity, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if userSubjectID == "" {
		return nil, ErrEmptySubjectID
	}

	var identities []*models.UserAuthIdentity
	err := s.db.
		Where("user_subject_id = ?", userSubjectID).
		Order("is_primary DESC, id ASC").
		Find(&identities).Error
	return identities, err
}

// ErrEmptySubjectID signals a caller-programming error (empty subject_id).
// Surfaced as a sentinel so handlers can map it to 400 without sniffing
// strings.
var ErrEmptySubjectID = errors.New("subject_id must not be empty")
