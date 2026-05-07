package user

import "gorm.io/gorm"

type Module struct {
	Service       *UserService
	CachedService *CachedUserService
}

func New(db *gorm.DB) *Module {
	svc := NewUserService(db)
	cached := NewCachedUserService(db)
	svc.SetOnUserUpdated(cached.InvalidateCache)
	return &Module{
		Service:       svc,
		CachedService: cached,
	}
}

func NewWithConfig(db *gorm.DB, syncIntervalMinutes int) *Module {
	svc := NewUserServiceWithConfig(db, syncIntervalMinutes)
	cached := NewCachedUserService(db)
	svc.SetOnUserUpdated(cached.InvalidateCache)
	return &Module{
		Service:       svc,
		CachedService: cached,
	}
}
