package user

import "gorm.io/gorm"

// Module groups user related services.
type Module struct {
	Service       *UserService
	CachedService *CachedUserService
}

// New creates a new user module.
func New(db *gorm.DB) *Module {
	return &Module{
		Service:       NewUserService(db),
		CachedService: NewCachedUserService(db),
	}
}
