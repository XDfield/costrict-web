// Package user — UserRef resolution (team-namespace API v1.1 §5.2).
//
// A UserRef is the shape callers pass when they want to identify a cs-user
// by EITHER the stable subject_id OR the upstream employee_number. Exactly
// one of the two must be set; both empty / both set is rejected with
// ErrInvalidUserRef (handler maps to HTTP 400 INVALID_USER_REF).
//
// Resolution flow:
//
//  1. UserID path:    GetUserByID(ref.UserID) → subject_id known → fetch binding.
//  2. EmployeeNumber: SearchByEmployeeNumber(ref.EmployeeNumber) → 0/1 row →
//     fetch binding for that subject_id. 0 rows → ErrUserNotFound (HTTP 404).
//  3. Once we have a subject_id, the local user_git_binding row resolves the
//     Git username. Missing row → ErrUserNotGiteaReady (HTTP 404, distinct
//     error code so the operator knows to run the cs-user
//     apply-enterprise-mapping flow rather than search for a missing user).
//
// This file deliberately does NOT trigger cs-user's lazy Gitea provisioning
// path — the team-namespace API contract treats that as an upstream
// responsibility. See plan note in buzzing-nibbling-kernighan.md.

package user

import (
	"context"
	"errors"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// UserRef is the input shape — exactly one of UserID / EmployeeNumber must
// be set. JSON tags mirror the team-namespace API v1.1 §5 contract so this
// struct can be embedded directly in handler request bodies.
type UserRef struct {
	UserID         string `json:"user_id,omitempty"`
	EmployeeNumber string `json:"employee_number,omitempty"`
}

// ResolvedUser is what UserRefResolver.Resolve returns. GitUsername is
// load-bearing — callers (team-namespace member sync) pass it straight into
// gitsync.Client.AddTeamMember.
type ResolvedUser struct {
	SubjectID   string
	GitUsername string
}

// ErrInvalidUserRef — neither or both of UserID / EmployeeNumber was set.
// Handlers translate to HTTP 400 with code INVALID_USER_REF.
var ErrInvalidUserRef = errors.New("userref: exactly one of user_id or employee_number is required")

// ErrUserNotGiteaReady — the resolved user has no Gitea binding yet. Handlers
// translate to HTTP 404 with code USER_NOT_GITEA_READY so the operator can
// distinguish from USER_NOT_FOUND (the cs-user lookup itself missed).
var ErrUserNotGiteaReady = errors.New("userref: user has no gitea binding; run cs-user apply-enterprise-mapping first")

// UserRefResolver resolves a UserRef to a git_username. Subject-id
// resolution (UserID / EmployeeNumber lookup) goes through cs-user RPC; the
// final binding lookup is a local query against server.user_git_binding
// (the row written by Phase 3's user.created event consumer on @server).
// Construct with NewUserRefResolver; nil RPC yields ErrRPCUnavailable on
// every call so handlers can return 503 cleanly.
//
// Phase 4 made the local binding path the only production wiring — main.go
// always wires SetLocalBindingDB; Resolve returns ErrNotConfigured if it
// wasn't called.
type UserRefResolver struct {
	rpc     *RPCClient
	localDB *gorm.DB
}

// NewUserRefResolver builds a resolver. rpc may be nil — Resolve then returns
// ErrRPCUnavailable so handlers degrade to 503 instead of panicking.
func NewUserRefResolver(rpc *RPCClient) *UserRefResolver {
	return &UserRefResolver{rpc: rpc}
}

// SetLocalBindingDB wires the local server DB used for user_git_binding
// lookups. Call this once at startup — main.go always wires it; calling
// with nil is a no-op.
func (r *UserRefResolver) SetLocalBindingDB(db *gorm.DB) {
	if r == nil {
		return
	}
	r.localDB = db
}

// Resolve validates ref, dispatches to the right cs-user lookup, then fetches
// the Gitea binding. Returns ResolvedUser with the gitea_username populated.
//
// Error mapping (consumed by handlers):
//
//	ErrInvalidUserRef       → 400 INVALID_USER_REF
//	ErrUserNotFound         → 404 USER_NOT_FOUND
//	ErrUserNotGiteaReady    → 404 USER_NOT_GITEA_READY
//	gorm.ErrRecordNotFound  → treated as ErrUserNotFound (defensive — RPC
//	                           surfaces 404 this way for by-id path)
//	ErrRPCUnavailable       → 503
//	ErrNotConfigured        → 503 (operator wiring issue)
func (r *UserRefResolver) Resolve(ctx context.Context, ref UserRef) (*ResolvedUser, error) {
	if r == nil || r.rpc == nil {
		return nil, ErrRPCUnavailable
	}
	if err := validateUserRef(ref); err != nil {
		return nil, err
	}

	subjectID, err := r.resolveSubjectID(ctx, ref)
	if err != nil {
		return nil, err
	}

	gitUsername, err := r.resolveGitUsername(ctx, subjectID)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(gitUsername) == "" {
		// Defensive: a synced-status row with an empty username means
		// provisioning was interrupted. Treat as not-ready.
		return nil, ErrUserNotGiteaReady
	}
	return &ResolvedUser{
		SubjectID:   subjectID,
		GitUsername: gitUsername,
	}, nil
}

// resolveGitUsername queries the local user_git_binding row for subjectID.
// Returns ErrUserNotGiteaReady if the row is missing — operator should run
// the cs-user apply-enterprise-mapping flow (the user.created event will
// provision the binding asynchronously).
func (r *UserRefResolver) resolveGitUsername(ctx context.Context, subjectID string) (string, error) {
	if r.localDB == nil {
		return "", ErrNotConfigured
	}
	var row models.UserGitBinding
	err := r.localDB.WithContext(ctx).
		Where("user_subject_id = ?", subjectID).
		First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", ErrUserNotGiteaReady
	}
	if err != nil {
		return "", err
	}
	return row.GitUsername, nil
}

// validateUserRef enforces the XOR constraint between UserID and EmployeeNumber.
func validateUserRef(ref UserRef) error {
	hasUserID := strings.TrimSpace(ref.UserID) != ""
	hasEmpNo := strings.TrimSpace(ref.EmployeeNumber) != ""
	if hasUserID == hasEmpNo {
		// Both empty OR both set — both are invalid per the contract.
		return ErrInvalidUserRef
	}
	return nil
}

// resolveSubjectID picks the right RPC method based on which field is set.
// Returns the cs-user subject_id; the caller then resolves the Gitea binding.
func (r *UserRefResolver) resolveSubjectID(ctx context.Context, ref UserRef) (string, error) {
	if strings.TrimSpace(ref.UserID) != "" {
		u, err := r.rpc.GetUserByID(ctx, ref.UserID)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return "", ErrUserNotFound
			}
			return "", err
		}
		return subjectIDFromUser(u), nil
	}

	// EmployeeNumber path.
	u, err := r.rpc.SearchByEmployeeNumber(ctx, ref.EmployeeNumber)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return "", err
		}
		return "", err
	}
	return subjectIDFromUser(u), nil
}

// subjectIDFromUser reads the stable identifier off a models.User. cs-user
// emits this as the SubjectID field; cs-user rows always have it populated
// (it's the primary key for cross-system correlation), so an empty value
// indicates an upstream bug we surface as ErrUserNotGiteaReady at the
// binding-lookup stage.
func subjectIDFromUser(u *models.User) string {
	if u == nil {
		return ""
	}
	return u.SubjectID
}
