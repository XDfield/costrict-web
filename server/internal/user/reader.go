package user

import (
	"context"

	"github.com/costrict/costrict-web/server/internal/models"
)

// UserReader is the read-only seam over user data. *UserService satisfies it
// directly (local DB); RPCClient (rpc_client.go) satisfies it over HTTP to the
// cs-user microservice. Module picks one based on USER_SERVICE_BACKEND.
type UserReader interface {
	GetUserByID(ctx context.Context, userID string) (*models.User, error)
	GetUsersByIDs(ctx context.Context, userIDs []string) (map[string]*models.User, error)
	SearchUsers(ctx context.Context, keyword string, limit int) ([]*models.User, error)
	ListUserIdentities(ctx context.Context, userSubjectID string) ([]*models.UserAuthIdentity, error)
}
