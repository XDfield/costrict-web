package leader

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"gorm.io/gorm"
)

func setupLeaderTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping leader election test")
	}
	db, err := database.Initialize(dsn)
	if err != nil {
		t.Fatalf("initialize database: %v", err)
	}
	return db
}

func TestElection_OnlyOneLeader(t *testing.T) {
	db := setupLeaderTestDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var leader1, leader2 int32

	e1 := NewElection(db, "costrict-test-leader", 1*time.Second)
	e2 := NewElection(db, "costrict-test-leader", 1*time.Second)

	go e1.Run(ctx, func() {
		atomic.StoreInt32(&leader1, 1)
	}, func() {
		atomic.StoreInt32(&leader1, 0)
	})

	go e2.Run(ctx, func() {
		atomic.StoreInt32(&leader2, 1)
	}, func() {
		atomic.StoreInt32(&leader2, 0)
	})

	// Wait for leadership to settle.
	time.Sleep(3 * time.Second)

	sum := atomic.LoadInt32(&leader1) + atomic.LoadInt32(&leader2)
	if sum != 1 {
		t.Fatalf("expected exactly one leader, got leader1=%d leader2=%d", atomic.LoadInt32(&leader1), atomic.LoadInt32(&leader2))
	}

	cancel()
	time.Sleep(1 * time.Second)
}

func TestElection_LeaderReleaseOnCancel(t *testing.T) {
	db := setupLeaderTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())

	var started, stopped int32
	e := NewElection(db, "costrict-test-leader-release", 500*time.Millisecond)

	go e.Run(ctx, func() {
		atomic.StoreInt32(&started, 1)
	}, func() {
		atomic.StoreInt32(&stopped, 1)
	})

	// Wait to become leader.
	time.Sleep(2 * time.Second)
	if atomic.LoadInt32(&started) != 1 {
		t.Fatal("expected to start as leader")
	}

	cancel()
	time.Sleep(1 * time.Second)
	if atomic.LoadInt32(&stopped) != 1 {
		t.Fatal("expected onStop to be called after cancel")
	}
}
