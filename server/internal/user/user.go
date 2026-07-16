package user

import (
	"log"

	"github.com/costrict/costrict-web/server/internal/config"
	"gorm.io/gorm"
)

// Module wires the user subsystem together. It exposes:
//   - Service: the local write-path service (always present; provides the
//     post-login hook surface and belt-and-suspenders writes during canary).
//   - CachedService: the read path used by handlers, wrapping whichever reader
//     was selected via cfg.Backend.
//   - Reader: the chosen read backend (either *UserService or *RPCClient).
//     Exposed so cmd/api can run boot-time validation.
//   - Writer: the chosen write backend. Resolves to *UserService when writes
//     stay local (WriteMode=local), to *RPCWriter when cs-user is the sole
//     write authority (WriteMode=readonly + Backend=rpc), or to a DualWriter
//     combining both for the rpc+local canary posture. Handlers must call
//     Writer.X — never Service.X directly — so the (Backend, WriteMode) env
//     vars are the single knob for write routing.
type Module struct {
	Service       *UserService
	CachedService *CachedUserService
	Reader        UserReader
	Writer        UserWriter
}

// New is the production convenience constructor: local backend, default sync
// interval.
func New(db *gorm.DB) *Module {
	return NewWithConfig(db, 0, config.UserServiceConfig{Backend: config.UserServiceBackendLocal})
}

// NewWithConfig builds the user module and selects the read+write backends
// based on cfg.Backend and cfg.WriteMode:
//
//	Backend | WriteMode | Reader     | Writer
//	--------+-----------+------------+---------------------------
//	local   | local     | UserService| UserService              (default)
//	local   | readonly  | UserService| (fatal — see validateUserConfig)
//	rpc     | local     | RPCClient  | DualWriter(svc, rpc)     (canary)
//	rpc     | readonly  | RPCClient  | RPCWriter                (cs-user authoritative)
//
// The local UserService is always constructed: it provides the post-login
// hook surface (SetPostLoginHook), backs the DualWriter's Primary side, and
// keeps the existing read fallback available. cfg.Backend only governs which
// reader the CachedService wraps.
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

	// Reader: UserService by default; RPCClient when Backend=rpc (and the
	// Phase 2 cs-user read API is wired).
	var reader UserReader = svc
	var rpcWriter *RPCWriter
	if cfg.Backend == config.UserServiceBackendRPC {
		rpc := NewRPCClient(cfg)
		if !rpc.Configured() {
			// Fail fast: operators flipped the backend flag but did not provide
			// URL/token. Better to refuse to start than to silently fall back
			// to local reads — a partial flip is a misconfiguration.
			log.Fatalf("user.NewWithConfig: USER_SERVICE_BACKEND=rpc but USER_SERVICE_URL or USER_SERVICE_INTERNAL_TOKEN is not set")
		}
		reader = rpc

		// Construct the RPCWriter regardless of WriteMode so DualWriter can
		// consume it. Configured() is implied true here because the RPCClient
		// passed the same checks above; NewRPCWriter never returns an error.
		rpcWriter = NewRPCWriter(cfg)
		if !rpcWriter.Configured() {
			// Defensive — same URL/token governs both; should be unreachable.
			log.Fatalf("user.NewWithConfig: USER_SERVICE_BACKEND=rpc but RPCWriter is not configured")
		}
	}

	// Writer: depends on (Backend, WriteMode). See the matrix above.
	var writer UserWriter = svc
	switch {
	case cfg.Backend == config.UserServiceBackendRPC && writeMode == config.UserServiceWriteModeReadonly:
		// cs-user is the sole write authority. Local writes are gated off
		// (UserService.SetWriteMode was called above), so writes through
		// Module.Writer bypass the local DB entirely.
		writer = rpcWriter
	case cfg.Backend == config.UserServiceBackendRPC && writeMode == config.UserServiceWriteModeLocal:
		// Dual-write canary: primary writes to local DB (authoritative),
		// secondary best-effort replicates to cs-user. Secondary failures are
		// logged but never fail the request — see DualWriter docs.
		writer = &DualWriter{Primary: svc, Secondary: rpcWriter}
	default:
		// Backend=local: writes stay on UserService. WriteMode is local here
		// because validateUserConfig fatals on local+readonly.
		writer = svc
	}

	cached := NewCachedUserService(reader)
	svc.SetOnUserUpdated(cached.InvalidateCache)

	return &Module{
		Service:       svc,
		CachedService: cached,
		Reader:        reader,
		Writer:        writer,
	}
}

// validateUserConfig inspects the (Backend, WriteMode) combination and returns
// (message, shouldFatal). Pure function so tests can exercise every combination
// without spawning subprocesses for log.Fatalf.
//
// Returns:
//   - ("", false) for the default local+local combo (no message, no fatal).
//   - (msg, false) for the dual-write canary combo (rpc+local) — warn only.
//   - (msg, true) for readonly+local — login broken, no benefit; refuse boot.
//   - ("", false) for readonly+rpc — cs-user Phase 2 write API is the writer.
//
// P0-8b unblocked readonly+rpc (previously fatal pre-Phase-2): RPCWriter is
// now constructed in NewWithConfig and routes writes to cs-user's Phase 2
// write API, so this combination is the production cutover posture.
func validateUserConfig(writeMode, backend string) (string, bool) {
	switch {
	case writeMode == config.UserServiceWriteModeReadonly && backend == config.UserServiceBackendLocal:
		// Writes are blocked but reads still go local — login is broken and
		// nothing in the system benefits. Refuse to start.
		return "USER_SERVICE_WRITE_MODE=readonly with USER_SERVICE_BACKEND=local — login is broken (writes have nowhere to go). Set USER_SERVICE_WRITE_MODE=local or USER_SERVICE_BACKEND=rpc.", true
	case writeMode == config.UserServiceWriteModeReadonly && backend == config.UserServiceBackendRPC:
		// cs-user Phase 2 write API is wired (RPCWriter). This is the
		// production cutover posture: cs-user is the sole write authority.
		// Allow boot without warning — this is the intended end state.
		return "", false
	case writeMode == config.UserServiceWriteModeLocal && backend == config.UserServiceBackendRPC:
		// Dual-write canary: writes go to both local DB (authoritative) and
		// cs-user (best-effort). Loud warning so operators don't run this in
		// a cutover without intending to — the local DB is still being
		// mutated, so it's not yet safe to flip a DB trigger that rejects
		// local writes.
		return "dual-write canary — USER_SERVICE_BACKEND=rpc with USER_SERVICE_WRITE_MODE=local. Writes go to both local DB and cs-user; local DB is still authoritative. Canary-only; do not flip the DB write trigger in this posture.", false
	default:
		return "", false
	}
}
