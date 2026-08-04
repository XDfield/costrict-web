package worker

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestPostgresGitSystemHookLockerMutualExclusion(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL advisory lock regression test")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	locker := newGitSystemHookLocker(db)
	serverID := "lock-test-" + uuid.NewString()

	cancelBeforeBegin, cancel := context.WithCancel(context.Background())
	cancel()
	lock, acquired, err := locker.TryLock(cancelBeforeBegin, serverID)
	if !errors.Is(err, context.Canceled) || acquired || lock != nil {
		t.Fatalf("cancelled TryLock: lock=%v acquired=%v err=%v", lock, acquired, err)
	}

	first, acquired, err := locker.TryLock(context.Background(), serverID)
	if err != nil || !acquired {
		t.Fatalf("first TryLock: acquired=%v err=%v", acquired, err)
	}
	second, acquired, err := locker.TryLock(context.Background(), serverID)
	if err != nil {
		t.Fatalf("second TryLock: %v", err)
	}
	if acquired || second != nil {
		t.Fatal("second connection acquired an already-held advisory lock")
	}
	if err := first.Finish(true); err != nil {
		t.Fatalf("commit first lock transaction: %v", err)
	}

	third, acquired, err := locker.TryLock(context.Background(), serverID)
	if err != nil || !acquired {
		t.Fatalf("third TryLock after release: acquired=%v err=%v", acquired, err)
	}
	if err := third.Finish(false); err != nil {
		t.Fatalf("rollback third lock transaction: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancelledLock, acquired, err := locker.TryLock(cancelCtx, serverID)
	if err != nil || !acquired {
		t.Fatalf("cancel-context TryLock: acquired=%v err=%v", acquired, err)
	}
	cancel()
	if err := cancelledLock.Finish(false); err != nil {
		t.Fatalf("finish cancelled lock transaction: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		lockAfterCancel, reacquired, lockErr := locker.TryLock(context.Background(), serverID)
		if lockErr != nil {
			t.Fatalf("TryLock after context cancellation: %v", lockErr)
		}
		if reacquired {
			if err := lockAfterCancel.Finish(false); err != nil {
				t.Fatalf("finish reacquired lock: %v", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("transaction-level advisory lock remained held after context cancellation")
		}
		time.Sleep(10 * time.Millisecond)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get PostgreSQL pool: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	occupiedConn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("occupy PostgreSQL connection: %v", err)
	}
	defer occupiedConn.Close()

	deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelDeadline()
	started := time.Now()
	lock, acquired, err = locker.TryLock(deadlineCtx, serverID)
	if !errors.Is(err, context.DeadlineExceeded) || acquired || lock != nil {
		t.Fatalf("connection-starved TryLock: lock=%v acquired=%v err=%v", lock, acquired, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("connection-starved TryLock returned too slowly: %s", elapsed)
	}
}
