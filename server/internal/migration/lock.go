// Package migration provides shared helpers for database migrations.
package migration

import (
	"fmt"
	"log"

	"gorm.io/gorm"
)

// PostgreSQL advisory lock keys used to ensure only one migration process
// runs at a time, even when multiple API/Worker replicas start concurrently.
const (
	LockKey1 = 12345
	LockKey2 = 67890
)

// AcquireLock obtains a PostgreSQL advisory lock for the migration process.
// The returned function must be called to release the lock.
func AcquireLock(db *gorm.DB) (func(), error) {
	if err := db.Exec("SELECT pg_advisory_lock(?, ?)", LockKey1, LockKey2).Error; err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	return func() {
		if err := db.Exec("SELECT pg_advisory_unlock(?, ?)", LockKey1, LockKey2).Error; err != nil {
			log.Printf("Failed to release migration lock: %v", err)
		}
	}, nil
}
