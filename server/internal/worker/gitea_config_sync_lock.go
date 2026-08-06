package worker

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"gorm.io/gorm"
)

// Advisory locking for GiteaConfigSyncWorker.
//
// Same mechanism as the Git system-hook reconciler (transaction-scoped
// pg_try_advisory_xact_lock on a per-server key, non-blocking so a replica that
// loses the race skips the round instead of queueing), but a SEPARATE key
// namespace on purpose: pushing quota rules and reconciling a webhook are
// independent operations, and sharing a key would make one silently starve the
// other on every tick.
//
// SQLite (used by tests) has no advisory locks, so the constructor degrades to
// a no-op locker there — single-process tests need no mutual exclusion.

// GiteaConfigSyncLock is one held lock. Finish commits or rolls back the
// transaction the lock lives in, releasing it either way.
type GiteaConfigSyncLock interface {
	Finish(success bool) error
}

// GiteaConfigSyncLocker hands out per-Git-server locks.
type GiteaConfigSyncLocker interface {
	TryLock(ctx context.Context, gitServerID string) (GiteaConfigSyncLock, bool, error)
}

type noOpGiteaConfigSyncLocker struct{}

type noOpGiteaConfigSyncLock struct{}

func (noOpGiteaConfigSyncLocker) TryLock(context.Context, string) (GiteaConfigSyncLock, bool, error) {
	return noOpGiteaConfigSyncLock{}, true, nil
}

func (noOpGiteaConfigSyncLock) Finish(bool) error { return nil }

type postgresGiteaConfigSyncLocker struct {
	DB *gorm.DB
}

type postgresGiteaConfigSyncLock struct {
	tx   *sql.Tx
	conn *sql.Conn
	once sync.Once
	err  error
}

func newGiteaConfigSyncLocker(db *gorm.DB) GiteaConfigSyncLocker {
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres" {
		return &postgresGiteaConfigSyncLocker{DB: db}
	}
	return noOpGiteaConfigSyncLocker{}
}

func (l *postgresGiteaConfigSyncLocker) TryLock(ctx context.Context, gitServerID string) (GiteaConfigSyncLock, bool, error) {
	if l == nil || l.DB == nil {
		return nil, false, errors.New("PostgreSQL Gitea config sync locker is not configured")
	}
	sqlDB, err := l.DB.DB()
	if err != nil {
		return nil, false, fmt.Errorf("get PostgreSQL connection pool: %w", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("get PostgreSQL advisory-lock connection: %w", err)
	}
	// context.WithoutCancel so a cancelled reconcile still gets an orderly
	// rollback in Finish rather than leaving the transaction to the pool.
	tx, err := conn.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("begin PostgreSQL advisory-lock transaction: %w", err)
	}

	key := giteaConfigSyncAdvisoryLockKey(gitServerID)
	var acquired bool
	if err := tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock($1)", key).Scan(&acquired); err != nil {
		_ = tx.Rollback()
		_ = conn.Close()
		return nil, false, fmt.Errorf("acquire PostgreSQL advisory lock: %w", err)
	}
	if !acquired {
		_ = tx.Rollback()
		_ = conn.Close()
		return nil, false, nil
	}
	return &postgresGiteaConfigSyncLock{tx: tx, conn: conn}, true, nil
}

func (l *postgresGiteaConfigSyncLock) Finish(success bool) error {
	if l == nil || l.tx == nil || l.conn == nil {
		return nil
	}
	l.once.Do(func() {
		var txErr error
		if success {
			txErr = l.tx.Commit()
		} else {
			txErr = l.tx.Rollback()
		}
		l.err = errors.Join(txErr, l.conn.Close())
	})
	return l.err
}

func giteaConfigSyncAdvisoryLockKey(gitServerID string) int64 {
	digest := sha256.Sum256([]byte("costrict:gitea-config-sync:" + gitServerID))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}
