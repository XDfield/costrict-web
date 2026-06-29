package leader

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"gorm.io/gorm"
)

// Election uses PostgreSQL advisory locks to elect a single leader among replicas.
// Only the replica that holds the advisory lock is considered leader and may run
// singleton background work.
type Election struct {
	db       *gorm.DB
	lockName string
	interval time.Duration
}

// NewElection creates an election for the given lock name.
// interval controls how often leadership is re-evaluated.
func NewElection(db *gorm.DB, lockName string, interval time.Duration) *Election {
	return &Election{db: db, lockName: lockName, interval: interval}
}

// Run blocks until ctx is done. It continuously tries to acquire/keep the advisory
// lock. When this replica becomes leader, onStart is called; when leadership is
// lost (lock released or ctx cancelled), onStop is called.
func (e *Election) Run(ctx context.Context, onStart, onStop func()) {
	var (
		mu     sync.Mutex
		leader bool
	)

	release := func(conn *sql.Conn, cancel context.CancelFunc) {
		mu.Lock()
		defer mu.Unlock()
		if !leader {
			return
		}
		if cancel != nil {
			cancel()
		}
		leader = false
		onStop()
		if conn != nil {
			_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", e.lockName)
		}
		log.Printf("leader: released %s", e.lockName)
	}

	ticker := time.NewTicker(e.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		mu.Lock()
		isLeader := leader
		mu.Unlock()

		if isLeader {
			// Already leader; the advisory lock is held on a dedicated connection.
			continue
		}

		sqlDB, err := e.db.DB()
		if err != nil {
			log.Printf("leader: failed to get sql.DB for %s: %v", e.lockName, err)
			continue
		}

		conn, err := sqlDB.Conn(ctx)
		if err != nil {
			log.Printf("leader: failed to acquire connection for %s: %v", e.lockName, err)
			continue
		}

		var acquired bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock(hashtext($1))", e.lockName).Scan(&acquired); err != nil {
			_ = conn.Close()
			log.Printf("leader: error acquiring lock %s: %v", e.lockName, err)
			continue
		}

		if !acquired {
			_ = conn.Close()
			continue
		}

		stopCtx, cancel := context.WithCancel(ctx)
		mu.Lock()
		leader = true
		mu.Unlock()
		log.Printf("leader: acquired %s", e.lockName)
		go onStart()

		// Watch for context cancellation and keep the connection alive.
		go func(c *sql.Conn, stop context.CancelFunc) {
			defer func() {
				release(c, stop)
				_ = c.Close()
			}()

			keepAlive := time.NewTicker(30 * time.Second)
			defer keepAlive.Stop()
			for {
				select {
				case <-stopCtx.Done():
					return
				case <-keepAlive.C:
					if _, err := c.ExecContext(context.Background(), "SELECT 1"); err != nil {
						log.Printf("leader: connection lost for %s: %v", e.lockName, err)
						return
					}
				}
			}
		}(conn, cancel)
	}
}
