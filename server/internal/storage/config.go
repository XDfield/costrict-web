package storage

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

const defaultLocalPath = "./data/artifacts"

type Config struct {
	Kind      string
	LocalPath string
	S3        S3Config
}

type S3Config struct {
	Endpoint       string
	Bucket         string
	Region         string
	ForcePathStyle bool
	CAFile         string
}

func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Kind:      strings.ToLower(strings.TrimSpace(envOrDefault("ARTIFACT_STORAGE_BACKEND", KindLocal))),
		LocalPath: envOrDefault("ARTIFACT_STORAGE_PATH", defaultLocalPath),
		S3: S3Config{
			Endpoint: strings.TrimSpace(os.Getenv("S3_ENDPOINT")),
			Bucket:   strings.TrimSpace(os.Getenv("S3_BUCKET")),
			Region:   strings.TrimSpace(os.Getenv("S3_REGION")),
			CAFile:   strings.TrimSpace(os.Getenv("S3_CA_FILE")),
		},
	}
	if value := strings.TrimSpace(os.Getenv("S3_FORCE_PATH_STYLE")); value != "" {
		forcePathStyle, err := strconv.ParseBool(value)
		if err != nil {
			return Config{}, fmt.Errorf("parse S3_FORCE_PATH_STYLE: %w", err)
		}
		cfg.S3.ForcePathStyle = forcePathStyle
	}
	return cfg, nil
}

func NewFromEnv(ctx context.Context) (*ConfiguredBackend, error) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		return nil, err
	}
	return NewFromConfig(ctx, cfg)
}

func NewFromConfig(ctx context.Context, cfg Config) (*ConfiguredBackend, error) {
	var backend Backend
	var err error
	switch cfg.Kind {
	case KindLocal:
		backend, err = NewLocalBackend(cfg.LocalPath)
	case KindS3:
		backend, err = NewS3Backend(ctx, cfg.S3)
	default:
		return nil, fmt.Errorf("unsupported artifact storage backend %q", cfg.Kind)
	}
	if err != nil {
		return nil, err
	}
	// Deployments have been bitten by config-precedence surprises (an in-container
	// .env silently losing to injected env vars), so state the effective backend
	// unambiguously at startup. Never log credentials here.
	switch cfg.Kind {
	case KindLocal:
		log.Printf("Artifact storage backend: local (path=%s)", cfg.LocalPath)
	case KindS3:
		log.Printf("Artifact storage backend: s3 (endpoint=%s, bucket=%s, region=%s, forcePathStyle=%v)",
			cfg.S3.Endpoint, cfg.S3.Bucket, cfg.S3.Region, cfg.S3.ForcePathStyle)
	}
	return &ConfiguredBackend{Kind: cfg.Kind, Backend: backend}, nil
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
