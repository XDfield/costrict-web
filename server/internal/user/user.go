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
// cfg.Backend, and the write gate based on cfg.WriteMode. Write path always
// stays on the local *UserService — the gate just decides whether write methods
// are allowed to execute or must short-circuit with ErrWriteBlocked.
func NewWithConfig(db *gorm.DB, syncIntervalMinutes int, cfg config.UserServiceConfig) *Module {
	// Default WriteMode to local if unset so a misconfigured env (typo, blank)
	// never silently blocks login.
	writeMode := cfg.WriteMode
	if writeMode != config.UserServiceWriteModeLocal && writeMode != config.UserServiceWriteModeReadonly {
		writeMode = config.UserServiceWriteModeLocal
	}

	if msg, fatal := validateUserConfig(writeMode, cfg.Backend); fatal {
		log.Fatalf("user.NewWithConfig: %s", msg)
	} else if msg != "" {
		log.Printf("[WARN] user.NewWithConfig: %s", msg)
	}

	svc := NewUserServiceWithConfig(db, syncIntervalMinutes)
	svc.SetWriteMode(writeMode)

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

// validateUserConfig inspects the (Backend, WriteMode) combination and returns
// (message, shouldFatal). Pure function so tests can exercise every combination
// without spawning subprocesses for log.Fatalf.
//
// Returns:
//   - ("", false) for the default local+local combo (no message, no fatal).
//   - (msg, false) for the canary split-brain combo (local+rpc) — warn only.
//   - (msg, true) for both readonly combos — login would be broken; refuse boot.
func validateUserConfig(writeMode, backend string) (string, bool) {
	switch {
	case writeMode == config.UserServiceWriteModeReadonly && backend == config.UserServiceBackendLocal:
		// Writes are blocked but reads still go local — login is broken and
		// nothing in the system benefits. Refuse to start.
		return "USER_SERVICE_WRITE_MODE=readonly with USER_SERVICE_BACKEND=local — login is broken (writes have nowhere to go). Set USER_SERVICE_WRITE_MODE=local or USER_SERVICE_BACKEND=rpc.", true
	case writeMode == config.UserServiceWriteModeReadonly && backend == config.UserServiceBackendRPC:
		// cs-user Phase 1 ships reads only; there is no write API yet. Until
		// Phase 2 lands, readonly+rpc also breaks login. Refuse to start so
		// operators get a clear message instead of runtime 500s on every login.
		return "USER_SERVICE_WRITE_MODE=readonly with USER_SERVICE_BACKEND=rpc — cs-user has no write API yet (Phase 2 pending); writes have nowhere to go. Keep USER_SERVICE_WRITE_MODE=local until cs-user Phase 2 ships.", true
	case writeMode == config.UserServiceWriteModeLocal && backend == config.UserServiceBackendRPC:
		// Split-brain: reads from cs-user, writes stay local. Acceptable for
		// read-only canary (verify RPC path under live traffic without
		// touching writes), but loud warning so it's not accidentally run in
		// a cutover.
		return "split-brain config — USER_SERVICE_BACKEND=rpc with USER_SERVICE_WRITE_MODE=local. Reads go to cs-user, writes stay local. Canary-only; do not run this in a prod cutover.", false
	default:
		return "", false
	}
}
