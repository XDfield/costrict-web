package user

import (
	"log"

	"github.com/costrict/costrict-web/server/internal/config"
	"gorm.io/gorm"
)

// Module wires the user subsystem together. It exposes:
//   - Service: the local write-path service (always present; cs-user has no
//     write API in Phase 1).
//   - CachedService: the read path used by handlers, wrapping whichever reader
//     was selected via cfg.Backend.
//   - Reader: the chosen read backend (either *UserService or *RPCClient).
//     Exposed so cmd/api can run boot-time validation.
type Module struct {
	Service       *UserService
	CachedService *CachedUserService
	Reader        UserReader
}

// New is the production convenience constructor: local backend, default sync
// interval.
func New(db *gorm.DB) *Module {
	return NewWithConfig(db, 0, config.UserServiceConfig{Backend: config.UserServiceBackendLocal})
}

// NewWithConfig builds the user module and selects the read backend based on
// cfg.Backend. Write path always stays on the local *UserService.
func NewWithConfig(db *gorm.DB, syncIntervalMinutes int, cfg config.UserServiceConfig) *Module {
	svc := NewUserServiceWithConfig(db, syncIntervalMinutes)

	var reader UserReader = svc
	if cfg.Backend == config.UserServiceBackendRPC {
		rpc := NewRPCClient(cfg)
		if !rpc.Configured() {
			// Fail fast: operators flipped the backend flag but did not provide
			// URL/token. Better to refuse to start than to silently fall back
			// to local reads — a partial flip is a misconfiguration.
			log.Fatalf("user.NewWithConfig: USER_SERVICE_BACKEND=rpc but USER_SERVICE_URL or USER_SERVICE_INTERNAL_TOKEN is not set")
		}
		reader = rpc
	}

	cached := NewCachedUserService(reader)
	svc.SetOnUserUpdated(cached.InvalidateCache)

	return &Module{
		Service:       svc,
		CachedService: cached,
		Reader:        reader,
	}
}
