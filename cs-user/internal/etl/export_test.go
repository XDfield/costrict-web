//go:build cgo

package etl

import (
	"context"
	"errors"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

func TestExportUsers_StreamingBatchOrder(t *testing.T) {
	db := newDB(t)
	for i := 0; i < 7; i++ {
		seedUser(t, db, freshUser(
			"subj-"+string(rune('a'+i)),
			"user-"+string(rune('a'+i)),
		))
	}

	var seen []string
	err := ExportUsers(context.Background(), db, 3, func(batch []*models.User) error {
		for _, u := range batch {
			seen = append(seen, u.SubjectID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExportUsers: %v", err)
	}
	if len(seen) != 7 {
		t.Errorf("saw %d rows, want 7", len(seen))
	}
	// Keyset pagination on id ASC means rows come out in insertion order.
	for i := 0; i < len(seen)-1; i++ {
		if seen[i] >= seen[i+1] {
			t.Errorf("rows out of order: %v", seen)
		}
	}
}

func TestExportUsers_IncludesSoftDeleted(t *testing.T) {
	db := newDB(t)
	seedUser(t, db, freshUser("alive", "alive"))
	// Insert normally then soft-delete (NOT Unscoped — that would hard-delete).
	u := freshUser("deleted", "deleted")
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.Delete(u).Error; err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	var seen []string
	if err := ExportUsers(context.Background(), db, 10, func(batch []*models.User) error {
		for _, u := range batch {
			seen = append(seen, u.SubjectID)
		}
		return nil
	}); err != nil {
		t.Fatalf("ExportUsers: %v", err)
	}
	if len(seen) != 2 {
		t.Errorf("expected 2 rows (incl soft-deleted), got %d: %v", len(seen), seen)
	}
}

func TestExportUsers_EmptyTableReturnsNil(t *testing.T) {
	db := newDB(t)
	called := false
	err := ExportUsers(context.Background(), db, 10, func(batch []*models.User) error {
		called = true
		return nil
	})
	if err != nil {
		t.Errorf("expected nil err, got %v", err)
	}
	if called {
		t.Errorf("callback should not be invoked on empty table")
	}
}

func TestExportUsers_BatchSizeOneExactlyCovers(t *testing.T) {
	db := newDB(t)
	for i := 0; i < 3; i++ {
		seedUser(t, db, freshUser("s"+itoa(i), "u"+itoa(i)))
	}
	batchCount := 0
	err := ExportUsers(context.Background(), db, 1, func(batch []*models.User) error {
		batchCount++
		if len(batch) != 1 {
			t.Errorf("batch %d: got %d rows, want 1", batchCount, len(batch))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ExportUsers: %v", err)
	}
	if batchCount != 3 {
		t.Errorf("expected 3 batches of 1, got %d", batchCount)
	}
}

func TestExportUsers_RejectsInvalidBatchSize(t *testing.T) {
	db := newDB(t)
	for _, size := range []int{0, -1, -10} {
		err := ExportUsers(context.Background(), db, size, func([]*models.User) error { return nil })
		if !errors.Is(err, ErrInvalidBatchSize) {
			t.Errorf("size=%d: expected ErrInvalidBatchSize, got %v", size, err)
		}
	}
}

func TestExportUsers_NilDBRejected(t *testing.T) {
	err := ExportUsers(context.Background(), nil, 10, func([]*models.User) error { return nil })
	if !errors.Is(err, ErrNilDB) {
		t.Errorf("expected ErrNilDB, got %v", err)
	}
}

func TestExportUsers_AbortStopsIteration(t *testing.T) {
	db := newDB(t)
	for i := 0; i < 10; i++ {
		seedUser(t, db, freshUser("s"+itoa(i), "u"+itoa(i)))
	}
	seen := 0
	err := ExportUsers(context.Background(), db, 2, func(batch []*models.User) error {
		seen += len(batch)
		if seen >= 4 {
			return ErrAbort
		}
		return nil
	})
	if err != nil {
		t.Errorf("abort should not surface as error, got %v", err)
	}
	if seen > 4 {
		t.Errorf("abort should have stopped iteration at ~4 rows, got %d", seen)
	}
}

func TestExportAuthIdentities_Streams(t *testing.T) {
	db := newDB(t)
	for i := 0; i < 5; i++ {
		seedAuthIdentity(t, db, freshAuthIdentity("k"+itoa(i), "subj", "casdoor"))
	}
	var seen []string
	if err := ExportAuthIdentities(context.Background(), db, 2, func(batch []*models.UserAuthIdentity) error {
		for _, ai := range batch {
			seen = append(seen, ai.ExternalKey)
		}
		return nil
	}); err != nil {
		t.Fatalf("ExportAuthIdentities: %v", err)
	}
	if len(seen) != 5 {
		t.Errorf("saw %d, want 5", len(seen))
	}
}

func TestCountUsers_ReturnsTotalIncludingSoftDeleted(t *testing.T) {
	db := newDB(t)
	seedUser(t, db, freshUser("a", "a"))
	seedUser(t, db, freshUser("b", "b"))
	u3 := freshUser("c", "c")
	if err := db.Create(u3).Error; err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := db.Delete(u3).Error; err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	n, err := CountUsers(context.Background(), db)
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n != 3 {
		t.Errorf("CountUsers = %d, want 3 (incl soft-deleted)", n)
	}
}

// itoa is a tiny local int->string helper to avoid pulling in strconv just
// for the loop counters above.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := false
	if i < 0 {
		neg = true
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
