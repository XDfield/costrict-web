package audit

import (
	"sync"
	"time"

	"github.com/costrict/costrict-web/proxy/internal/logger"
)

type Worker struct {
	store        AuditStore
	ch           chan *AuditLog
	batchSize    int
	flushMs      int
	retentionDays int
	done         chan struct{}
	wg           sync.WaitGroup
}

func NewWorker(store AuditStore, channelSize, batchSize, flushMs, retentionDays int) *Worker {
	return &Worker{
		store:         store,
		ch:            make(chan *AuditLog, channelSize),
		batchSize:     batchSize,
		flushMs:       flushMs,
		retentionDays: retentionDays,
		done:          make(chan struct{}),
	}
}

func (w *Worker) Start() {
	w.wg.Add(1)
	go w.run()
	w.wg.Add(1)
	go w.retentionLoop()
}

func (w *Worker) Stop() {
	close(w.done)
	close(w.ch)
	w.wg.Wait()
}

func (w *Worker) Send(entry *AuditLog) {
	select {
	case w.ch <- entry:
	default:
		logger.Warn("audit channel full, dropping entry for path %s", entry.ApiPath)
	}
}

func (w *Worker) run() {
	defer w.wg.Done()

	batch := make([]*AuditLog, 0, w.batchSize)
	ticker := time.NewTicker(time.Duration(w.flushMs) * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case entry, ok := <-w.ch:
			if !ok {
				if len(batch) > 0 {
					w.flush(batch)
				}
				return
			}
			batch = append(batch, entry)
			if len(batch) >= w.batchSize {
				w.flush(batch)
				batch = make([]*AuditLog, 0, w.batchSize)
			}
		case <-ticker.C:
			if len(batch) > 0 {
				w.flush(batch)
				batch = make([]*AuditLog, 0, w.batchSize)
			}
		}
	}
}

func (w *Worker) flush(batch []*AuditLog) {
	for retry := 0; retry < 3; retry++ {
		if err := w.store.InsertBatch(batch); err != nil {
			logger.Error("audit batch write failed (retry %d): %v", retry+1, err)
			time.Sleep(time.Second * time.Duration(retry+1))
			continue
		}
		return
	}
	logger.Error("audit batch write failed after 3 retries, dropping %d entries", len(batch))
}

func (w *Worker) retentionLoop() {
	defer w.wg.Done()

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-w.done:
			return
		case <-ticker.C:
			cutoff := time.Now().AddDate(0, 0, -w.retentionDays)
			if err := w.store.CleanBefore(cutoff); err != nil {
				logger.Error("audit retention cleanup failed: %v", err)
			}
		}
	}
}
