package storage

import (
	"context"
	"strings"
	"testing"
)

func TestNewFromConfigRejectsUnknownBackend(t *testing.T) {
	_, err := NewFromConfig(context.Background(), Config{Kind: "unknown"})
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("expected unsupported backend error, got %v", err)
	}
}

func TestNewFromConfigRequiresS3EndpointAndBucket(t *testing.T) {
	tests := []struct {
		name string
		cfg  S3Config
		want string
	}{
		{name: "endpoint", cfg: S3Config{Bucket: "artifacts", Region: "internal"}, want: "S3_ENDPOINT"},
		{name: "bucket", cfg: S3Config{Endpoint: "http://127.0.0.1", Region: "internal"}, want: "S3_BUCKET"},
		{name: "region", cfg: S3Config{Endpoint: "http://127.0.0.1", Bucket: "artifacts"}, want: "S3_REGION"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewFromConfig(context.Background(), Config{Kind: KindS3, S3: test.cfg})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %s validation error, got %v", test.want, err)
			}
		})
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("ARTIFACT_STORAGE_BACKEND", " S3 ")
	t.Setenv("S3_ENDPOINT", "http://object-store.internal")
	t.Setenv("S3_BUCKET", "artifacts")
	t.Setenv("S3_REGION", "internal")
	t.Setenv("S3_FORCE_PATH_STYLE", "true")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kind != KindS3 || cfg.S3.Endpoint != "http://object-store.internal" ||
		cfg.S3.Bucket != "artifacts" || cfg.S3.Region != "internal" ||
		!cfg.S3.ForcePathStyle {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestConfiguredBackendKind(t *testing.T) {
	local, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configured := &ConfiguredBackend{Kind: KindS3, Backend: local}
	if got := KindOf(configured); got != KindS3 {
		t.Fatalf("KindOf() = %q, want %q", got, KindS3)
	}
	if got := KindOf(local); got != KindLocal {
		t.Fatalf("legacy KindOf() = %q, want %q", got, KindLocal)
	}
}

func TestValidateRecordedBackend(t *testing.T) {
	local, err := NewLocalBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	configuredLocal := &ConfiguredBackend{Kind: KindLocal, Backend: local}
	configuredS3 := &ConfiguredBackend{Kind: KindS3, Backend: local}

	if err := ValidateRecordedBackend("", configuredLocal); err != nil {
		t.Fatalf("legacy empty backend should remain compatible with local storage: %v", err)
	}
	if err := ValidateRecordedBackend("", configuredS3); err == nil {
		t.Fatal("legacy empty backend should be rejected for S3 storage")
	}
	if err := ValidateRecordedBackend("", nil); err == nil {
		t.Fatal("legacy empty backend must still require configured storage")
	}
	if err := ValidateRecordedBackend(KindS3, configuredS3); err != nil {
		t.Fatalf("matching backend should be accepted: %v", err)
	}
	if err := ValidateRecordedBackend(KindLocal, configuredS3); err == nil {
		t.Fatal("mismatched backend should be rejected")
	}
}
