package adminimport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/storage"
)

type importStorageBackend struct {
	mu      sync.Mutex
	objects map[string][]byte
	gets    int
}

func newImportStorageBackend() *importStorageBackend {
	return &importStorageBackend{objects: make(map[string][]byte)}
}

func (b *importStorageBackend) Put(_ context.Context, key string, reader io.Reader, _ int64) error {
	content, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.objects[key] = content
	b.mu.Unlock()
	return nil
}

func (b *importStorageBackend) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.gets++
	content, ok := b.objects[key]
	if !ok {
		return nil, 0, fmt.Errorf("object %q not found", key)
	}
	copyOfContent := append([]byte(nil), content...)
	return io.NopCloser(bytes.NewReader(copyOfContent)), int64(len(copyOfContent)), nil
}

func (b *importStorageBackend) getCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gets
}

func TestCreateUploadJobRecordsConfiguredBackend(t *testing.T) {
	db := setupTestDB(t)
	objects := newImportStorageBackend()
	backend := &storage.ConfiguredBackend{Kind: storage.KindS3, Backend: objects}
	svc := NewService(db, backend)
	content := []byte("catalog bundle")

	job, err := svc.CreateUploadJob(
		context.Background(),
		bytes.NewReader(content),
		int64(len(content)),
		"catalog.tar.gz",
		false,
		"admin",
	)
	if err != nil {
		t.Fatalf("create upload job: %v", err)
	}
	if job.StorageBackend != storage.KindS3 {
		t.Fatalf("job storage backend = %q, want %q", job.StorageBackend, storage.KindS3)
	}

	var persisted models.CapabilityImportJob
	if err := db.First(&persisted, "id = ?", job.ID).Error; err != nil {
		t.Fatalf("load upload job: %v", err)
	}
	if persisted.StorageBackend != storage.KindS3 || persisted.StorageKey == "" {
		t.Fatalf("persisted storage mapping is incomplete: %+v", persisted)
	}

	runner := NewImportRunner(db, backend, nil)
	source, cleanup, err := runner.materialize(context.Background(), &persisted)
	if err != nil {
		t.Fatalf("materialize matching S3 job: %v", err)
	}
	defer cleanup()
	got, err := os.ReadFile(source.Tarball)
	if err != nil {
		t.Fatalf("read materialized bundle: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("materialized bundle = %q, want %q", got, content)
	}
}

func TestMaterializeRejectsBackendMismatchBeforeGet(t *testing.T) {
	objects := newImportStorageBackend()
	objects.objects["import-jobs/job-1/bundle.tar.gz"] = []byte("must not be read")
	backend := &storage.ConfiguredBackend{Kind: storage.KindS3, Backend: objects}
	runner := NewImportRunner(setupTestDB(t), backend, nil)
	job := &models.CapabilityImportJob{
		ID:             "job-1",
		SourceKind:     sourceKindUpload,
		StorageBackend: storage.KindLocal,
		StorageKey:     "import-jobs/job-1/bundle.tar.gz",
	}

	if _, _, err := runner.materialize(context.Background(), job); err == nil {
		t.Fatal("expected backend mismatch to reject materialization")
	}
	if got := objects.getCount(); got != 0 {
		t.Fatalf("backend mismatch must not call Get, got %d calls", got)
	}
}

func TestMaterializeTreatsLegacyEmptyBackendAsLocal(t *testing.T) {
	objects := newImportStorageBackend()
	key := "import-jobs/legacy/bundle.tar.gz"
	objects.objects[key] = []byte("legacy local bundle")
	runner := NewImportRunner(setupTestDB(t), objects, nil)
	job := &models.CapabilityImportJob{
		ID:         "legacy",
		SourceKind: sourceKindUpload,
		StorageKey: key,
	}

	source, cleanup, err := runner.materialize(context.Background(), job)
	if err != nil {
		t.Fatalf("materialize legacy local job: %v", err)
	}
	defer cleanup()
	content, err := os.ReadFile(source.Tarball)
	if err != nil {
		t.Fatalf("read legacy bundle: %v", err)
	}
	if string(content) != "legacy local bundle" {
		t.Fatalf("legacy bundle = %q", content)
	}
}
