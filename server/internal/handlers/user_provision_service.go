// Package handlers — accessor for the singleton UserProvisionService.
//
// Phase 1 keeps wiring minimal: main.go constructs a single
// gitsync.UserProvisionService and registers it via InitUserProvisionService;
// Phase 2's event consumer endpoint will retrieve it via
// GetUserProvisionService to drive Gitea account provisioning from
// cs-user user.created events.

package handlers

import (
	"sync"

	"github.com/costrict/costrict-web/server/internal/gitsync"
)

var (
	userProvisionSvcMu sync.RWMutex
	userProvisionSvc   *gitsync.UserProvisionService
)

// InitUserProvisionService registers the singleton UserProvisionService.
// Safe to call once at boot. Passing nil clears the registration.
func InitUserProvisionService(svc *gitsync.UserProvisionService) {
	userProvisionSvcMu.Lock()
	defer userProvisionSvcMu.Unlock()
	userProvisionSvc = svc
}

// GetUserProvisionService returns the registered service or nil if Phase 1
// wiring is not active (e.g. tests that didn't call Init).
func GetUserProvisionService() *gitsync.UserProvisionService {
	userProvisionSvcMu.RLock()
	defer userProvisionSvcMu.RUnlock()
	return userProvisionSvc
}
