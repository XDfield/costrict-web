package user

import "errors"

// WriteMode values mirror config.UserServiceWriteMode* but live in the user
// package so service.go can compare without importing config (which would
// couple the lower-level service layer to the config layer).
const (
	WriteModeLocal    = "local"
	WriteModeReadonly = "readonly"
)

// ErrWriteBlocked is returned by UserService write methods when
// USER_SERVICE_WRITE_MODE=readonly. The gate is a kill switch for the P0-8
// READONLY cutover: until cs-user Phase 2 ships a write API, flipping this
// mode breaks every login / bind / unbind path, so the only safe deployment
// is the default (WriteMode=local).
var ErrWriteBlocked = errors.New("user service: write blocked (USER_SERVICE_WRITE_MODE=readonly)")
