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

type GitSystemHookLock interface {
	Finish(success bool) error
}

type GitSystemHookLocker interface {
	TryLock(ctx context.Context, gitServerID string) (GitSystemHookLock, bool, error)
}

type noOpGitSystemHookLocker struct{}

type noOpGitSystemHookLock struct{}

func (noOpGitSystemHookLocker) TryLock(context.Context, string) (GitSystemHookLock, bool, error) {
	return noOpGitSystemHookLock{}, true, nil
}

func (noOpGitSystemHookLock) Finish(bool) error { return nil }

type postgresGitSystemHookLocker struct {
	DB *gorm.DB
}

type postgresGitSystemHookLock struct {
	tx   *sql.Tx
	conn *sql.Conn
	once sync.Once
	err  error
}

func newGitSystemHookLocker(db *gorm.DB) GitSystemHookLocker {
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "postgres" {
		return &postgresGitSystemHookLocker{DB: db}
	}
	return noOpGitSystemHookLocker{}
}

func (l *postgresGitSystemHookLocker) TryLock(ctx context.Context, gitServerID string) (GitSystemHookLock, bool, error) {
	if l == nil || l.DB == nil {
		return nil, false, errors.New("PostgreSQL Git system hook locker is not configured")
	}
	sqlDB, err := l.DB.DB()
	if err != nil {
		return nil, false, fmt.Errorf("get PostgreSQL connection pool: %w", err)
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("get PostgreSQL advisory-lock connection: %w", err)
	}
	tx, err := conn.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		_ = conn.Close()
		return nil, false, fmt.Errorf("begin PostgreSQL advisory-lock transaction: %w", err)
	}

	key := gitSystemHookAdvisoryLockKey(gitServerID)
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
	return &postgresGitSystemHookLock{tx: tx, conn: conn}, true, nil
}

func (l *postgresGitSystemHookLock) Finish(success bool) error {
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

func gitSystemHookAdvisoryLockKey(gitServerID string) int64 {
	digest := sha256.Sum256([]byte("costrict:git-system-hook:" + gitServerID))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}
