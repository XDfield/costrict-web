package user

import "gorm.io/gorm"

// Module groups user related services.
type Module struct {
	Service       *UserService
	CachedService *CachedUserService
}

// New creates a new user module with default sync interval (15 minutes).
func New(db *gorm.DB) *Module {
	return &Module{
		Service:       NewUserService(db),
		CachedService: NewCachedUserService(db),
	}
}

// NewWithConfig creates a new user module with custom sync interval.
func NewWithConfig(db *gorm.DB, syncIntervalMinutes int) *Module {
	return &Module{
		Service:       NewUserServiceWithConfig(db, syncIntervalMinutes),
		CachedService: NewCachedUserService(db),
	}
}
