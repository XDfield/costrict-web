package audit

import (
	"testing"
	"time"
)

type mockStore struct {
	batches [][]*AuditLog
	err     error
}

func (m *mockStore) InsertBatch(entries []*AuditLog) error {
	m.batches = append(m.batches, entries)
	return m.err
}

func (m *mockStore) CleanBefore(before time.Time) error {
	return nil
}

func TestWorker_BatchAggregation(t *testing.T) {
	store := &mockStore{}
	w := NewWorker(store, 100, 10, 50, 90)
	w.Start()

	for i := 0; i < 5; i++ {
		w.Send(NewAuditLog())
	}

	time.Sleep(100 * time.Millisecond)
	w.Stop()

	if len(store.batches) == 0 {
		t.Fatal("expected at least one batch")
	}

	total := 0
	for _, b := range store.batches {
		total += len(b)
	}
	if total != 5 {
		t.Errorf("expected 5 entries, got %d", total)
	}
}

func TestWorker_FullBatch(t *testing.T) {
	store := &mockStore{}
	w := NewWorker(store, 100, 3, 5000, 90)
	w.Start()

	for i := 0; i < 6; i++ {
		w.Send(NewAuditLog())
	}

	time.Sleep(200 * time.Millisecond)
	w.Stop()

	if len(store.batches) < 2 {
		t.Errorf("expected at least 2 batches for 6 items with batch size 3, got %d", len(store.batches))
	}
}

func TestWorker_WriteError(t *testing.T) {
	store := &mockStore{err: ErrTestFailed}
	w := NewWorker(store, 100, 1, 50, 90)
	w.Start()

	w.Send(NewAuditLog())

	time.Sleep(500 * time.Millisecond)
	w.Stop()
}

var ErrTestFailed = &testError{}

type testError struct{}

func (e *testError) Error() string { return "test write failed" }
