package services

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// bundleArtifactSourceType marks artifacts produced by the lazy clone-and-pack
// pipeline, distinguishing them from user "upload" artifacts that come from
// ParseArchive. The DB+HTTP distribution channel only ever serves the latest
// clone_pack artifact for a plugin.
// BundleSourceTypeClonePack marks bundle artifacts produced by the lazy
// clone-and-pack pipeline (catalog-ingested plugins).
const BundleSourceTypeClonePack = "clone_pack"

// BundleSourceTypeUploadPack marks bundle artifacts produced synchronously from an
// uploaded plugin's already-stored capability_assets (no clone needed). Uploaded
// plugins go through ParseArchive which stores every file, so reconstruction is
// lossless and on-demand — the bundle HTTP endpoint can serve them immediately.
const BundleSourceTypeUploadPack = "upload_pack"

// BundleSourceTypeSeeded marks bundle artifacts written directly from a pre-packed
// plugin ZIP carried inside an offline / air-gap catalog bundle (PR5, design §12 +
// ADR-8). The ingest reads the ZIP and version key straight out of the bundle and
// writes the artifact, SKIPPING the lazy clone-and-pack entirely. A seeded artifact
// is a terminal state for that version: no clone, no refresh enqueue. It is served
// by the bundle endpoint IDENTICALLY to clone_pack/upload_pack (it is in
// BundleSourceTypes), so csc sees zero difference between online and offline modes.
const BundleSourceTypeSeeded = "seeded"

const bundleArtifactSourceType = BundleSourceTypeClonePack
const uploadBundleSourceType = BundleSourceTypeUploadPack

// BundleSourceTypes is the set of source types the DB+HTTP distribution channel
// serves. All three produce a lossless ZIP: clone_pack is for catalog-ingested
// plugins (lazy clone), upload_pack is for user-uploaded plugins (asset
// reconstruction), and seeded is for offline-seeded plugins (pre-packed ZIP from
// an air-gap bundle). Adding seeded here is what makes the bundle endpoint and
// ItemResponse treat it as a valid bundle WITHOUT any endpoint-side special case.
var BundleSourceTypes = []string{BundleSourceTypeClonePack, BundleSourceTypeUploadPack, BundleSourceTypeSeeded}

// seedBundleUploader is the synthetic "uploader" recorded on offline-seeded bundle
// artifacts (UploadedBy is NOT NULL on the model). Distinct from the lazy-clone
// uploader so the provenance is greppable.
const seedBundleUploader = "system:seed"

// bundleArtifactUploader is the synthetic "uploader" recorded on system-produced
// bundle artifacts (UploadedBy is NOT NULL on the model).
const bundleArtifactUploader = "system:bundle-pack"

// bundleMimeType is the content type stored on bundle artifacts. The bundle format
// is ZIP (design decision D1), not tar.gz: csc already has a mature ZIP extraction
// path (exec-bit-preserving) and the backend produces ZIP via archive/zip.
const bundleMimeType = "application/zip"

// BundlePackService produces a lossless ZIP "bundle" artifact for a plugin item by
// lazily cloning its upstream source and packing the working tree. It is the
// synchronous core of the DB+HTTP "subscribe-to-distribute" channel; the async
// worker, queueing, and HTTP download endpoint are layered on top in PR2b.
//
// Dependencies are injected (DB, GitService, storage.Backend) rather than reaching
// into the handlers package, to avoid a services->handlers import cycle. This
// mirrors how SyncService carries its collaborators as struct fields.
type BundlePackService struct {
	DB         *gorm.DB
	Git        *GitService
	Storage    storage.Backend
	MirrorBase string // GIT_MIRROR_BASE; empty = direct GitHub clone (no rewrite)
	// AllowLocalClone permits file:// / local-directory clone targets. It defaults
	// to false (production-safe: only http(s) sources are cloned, blocking a
	// file://-based SSRF/local-repo-disclosure via a tampered catalog entry) and is
	// set true only by unit tests that clone from a temp git repo without network.
	AllowLocalClone bool
}

// NewBundlePackService constructs a BundlePackService with the given collaborators.
func NewBundlePackService(db *gorm.DB, git *GitService, store storage.Backend, mirrorBase string) *BundlePackService {
	return &BundlePackService{DB: db, Git: git, Storage: store, MirrorBase: mirrorBase}
}

// PackItemBundle clones the item's upstream source, packs the working tree into a
// lossless ZIP, stores it, and upserts a `clone_pack` CapabilityArtifact whose
// ArtifactVersion is the upstream git commit SHA (design decision D3 — NEVER the
// item's SourceSHA, which only hashes the synthetic main file and misses
// hooks/scripts).
//
// It is idempotent: if a `clone_pack` artifact already exists for the current
// upstream commit SHA and its stored file is present, the existing artifact is
// returned without re-cloning or re-writing. (The SHA is only known after cloning,
// so the order is clone -> compare -> short-circuit; this is acceptable per the
// task design.)
//
// On success the produced artifact is marked IsLatest=true and any older
// `clone_pack` artifact for the same item is demoted to IsLatest=false.
func (s *BundlePackService) PackItemBundle(ctx context.Context, item *models.CapabilityItem) (*models.CapabilityArtifact, error) {
	if item == nil {
		return nil, fmt.Errorf("bundle pack: item is nil")
	}
	if item.SourceURL == "" {
		return nil, fmt.Errorf("bundle pack: item %s has no source_url", item.ID)
	}

	cloneURL, branch, subPath, err := parseSourceURL(item.SourceURL)
	if err != nil {
		return nil, fmt.Errorf("bundle pack: parse source_url for item %s: %w", item.ID, err)
	}
	cloneURL = mapToMirror(cloneURL, s.MirrorBase)

	// Defense-in-depth: refuse to server-side clone anything but an http(s) remote
	// (no file:// / local-path SSRF into a publicly downloadable bundle). Tests opt
	// into local clones via AllowLocalClone; production never sets it.
	if err := validateCloneURL(cloneURL, s.AllowLocalClone); err != nil {
		return nil, fmt.Errorf("bundle pack: %w", err)
	}

	clone, err := s.Git.Clone(cloneURL, branch)
	if err != nil {
		return nil, fmt.Errorf("bundle pack: clone %s (branch=%q): %w", cloneURL, branch, err)
	}
	defer func() {
		if cleanupErr := s.Git.Cleanup(clone.LocalPath); cleanupErr != nil {
			logger.Warn("[bundle-pack] cleanup temp clone %s failed: %v", clone.LocalPath, cleanupErr)
		}
	}()

	commitSHA := clone.CommitSHA
	if commitSHA == "" {
		return nil, fmt.Errorf("bundle pack: empty commit SHA after cloning %s", cloneURL)
	}

	// Idempotency: a fresh bundle for this exact commit SHA already cached?
	if existing, ok := s.findCachedBundle(ctx, item.ID, commitSHA); ok {
		logger.Info("[bundle-pack] item %s already packed at %s, reusing artifact %s", item.ID, commitSHA, existing.ID)
		return existing, nil
	}

	zipBytes, zipSHA, err := s.Git.PackZip(clone.LocalPath, subPath)
	if err != nil {
		return nil, fmt.Errorf("bundle pack: pack zip for item %s: %w", item.ID, err)
	}

	repoID := s.resolveRepoID(item)
	storageKey := bundleStorageKey(repoID, item.ID, commitSHA)

	artifact, err := s.storeBundleArtifact(ctx, storeBundleArtifactInput{
		item:       item,
		zipBytes:   zipBytes,
		zipSHA:     zipSHA,
		version:    commitSHA,
		storageKey: storageKey,
		sourceType: bundleArtifactSourceType,
		uploadedBy: bundleArtifactUploader,
	})
	if err != nil {
		return nil, fmt.Errorf("bundle pack: %w", err)
	}

	logger.Info("[bundle-pack] packed item %s slug=%s sha=%s size=%d bytes", item.ID, item.Slug, commitSHA, len(zipBytes))
	return artifact, nil
}

// storeBundleArtifactInput carries everything storeBundleArtifact needs to write a
// bundle artifact. The three callers (clone_pack, upload_pack, seeded) differ only
// in the source type, version key, and storage key — the storage Put + IsLatest
// demote/create transaction + orphan cleanup is identical, so it lives here once.
type storeBundleArtifactInput struct {
	item       *models.CapabilityItem
	zipBytes   []byte
	zipSHA     string
	version    string
	storageKey string
	sourceType string
	uploadedBy string
}

// storeBundleArtifact puts the ZIP bytes into the storage backend, then upserts a
// CapabilityArtifact in one transaction that first demotes any previous IsLatest
// artifact for this item+source_type so only the new bundle is IsLatest (mirrors the
// is_latest mutual-exclusion update in UploadArtifact, scoped to bundle artifacts).
// On a failed DB write it best-effort deletes the orphaned stored file.
func (s *BundlePackService) storeBundleArtifact(ctx context.Context, in storeBundleArtifactInput) (*models.CapabilityArtifact, error) {
	if err := s.Storage.Put(ctx, in.storageKey, bytes.NewReader(in.zipBytes), int64(len(in.zipBytes))); err != nil {
		return nil, fmt.Errorf("store bundle for item %s: %w", in.item.ID, err)
	}

	artifact := &models.CapabilityArtifact{
		ID:              uuid.New().String(),
		ItemID:          in.item.ID,
		Filename:        bundleFilename(in.item.Slug),
		FileSize:        int64(len(in.zipBytes)),
		ChecksumSHA256:  in.zipSHA,
		MimeType:        bundleMimeType,
		StorageBackend:  "local",
		StorageKey:      in.storageKey,
		ArtifactVersion: in.version,
		IsLatest:        true,
		SourceType:      in.sourceType,
		UploadedBy:      in.uploadedBy,
		CreatedAt:       time.Now(),
	}

	if err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.CapabilityArtifact{}).
			Where("item_id = ? AND source_type = ?", in.item.ID, in.sourceType).
			Update("is_latest", false).Error; err != nil {
			return err
		}
		return tx.Create(artifact).Error
	}); err != nil {
		// Best-effort cleanup of the orphaned stored file.
		if delErr := s.Storage.Delete(context.Background(), in.storageKey); delErr != nil {
			logger.Warn("[bundle-pack] orphan bundle cleanup %s failed: %v", in.storageKey, delErr)
		}
		return nil, fmt.Errorf("persist artifact for item %s: %w", in.item.ID, err)
	}
	return artifact, nil
}

// findCachedBundle returns the existing latest-or-versioned clone_pack artifact for
// the given commit SHA when its stored file is still present.
func (s *BundlePackService) findCachedBundle(ctx context.Context, itemID, commitSHA string) (*models.CapabilityArtifact, bool) {
	var existing models.CapabilityArtifact
	err := s.DB.WithContext(ctx).
		Where("item_id = ? AND source_type = ? AND artifact_version = ?", itemID, bundleArtifactSourceType, commitSHA).
		Order("created_at DESC").
		First(&existing).Error
	if err != nil {
		return nil, false
	}
	present, existsErr := s.Storage.Exists(ctx, existing.StorageKey)
	if existsErr != nil || !present {
		return nil, false
	}
	return &existing, true
}

// resolveRepoID returns the repo ID for storage-key namespacing, loading the
// Registry if it was not preloaded. Falls back to "public" so the key is always
// well-formed.
func (s *BundlePackService) resolveRepoID(item *models.CapabilityItem) string {
	if item.Registry != nil && item.Registry.RepoID != "" {
		return item.Registry.RepoID
	}
	if item.RegistryID != "" {
		var reg models.CapabilityRegistry
		if err := s.DB.Select("repo_id").First(&reg, "id = ?", item.RegistryID).Error; err == nil && reg.RepoID != "" {
			return reg.RepoID
		}
	}
	return "public"
}

// bundleStorageKey builds the storage key for a plugin bundle:
//
//	<repoID>/<itemID>/bundle/<commitSHA>.zip
func bundleStorageKey(repoID, itemID, commitSHA string) string {
	if repoID == "" {
		repoID = "public"
	}
	return fmt.Sprintf("%s/%s/bundle/%s.zip", repoID, itemID, commitSHA)
}

// bundleFilename is the download filename advertised on the artifact.
func bundleFilename(slug string) string {
	if slug == "" {
		slug = "plugin"
	}
	return slug + ".zip"
}

// PackUploadBundle builds a lossless ZIP for an *uploaded* plugin directly from its
// already-stored capability_assets (and item.Content as a fallback for the main
// file), then upserts an `upload_pack` CapabilityArtifact and returns it.
//
// Uploaded plugins go through ParseArchive, which persists every file, so no clone
// is needed — this is the synchronous counterpart to PackItemBundle and gives the
// bundle HTTP endpoint a single uniform artifact shape across catalog and upload
// plugins (acceptance criterion: "upload plugin and catalog plugin go through the
// same bundle interface, both lossless").
//
// The artifact version is the sha256 of the produced ZIP bytes (deterministic
// content hash — there is no upstream commit SHA for an uploaded plugin). Like
// PackItemBundle it is idempotent on (item, version) and flips IsLatest.
func (s *BundlePackService) PackUploadBundle(ctx context.Context, item *models.CapabilityItem) (*models.CapabilityArtifact, error) {
	if item == nil {
		return nil, fmt.Errorf("upload bundle: item is nil")
	}

	var assets []models.CapabilityAsset
	if err := s.DB.WithContext(ctx).Where("item_id = ?", item.ID).Order("rel_path asc").Find(&assets).Error; err != nil {
		return nil, fmt.Errorf("upload bundle: load assets for item %s: %w", item.ID, err)
	}

	zipBytes, zipSHA, err := s.packAssetsZip(ctx, item, assets)
	if err != nil {
		return nil, fmt.Errorf("upload bundle: pack assets for item %s: %w", item.ID, err)
	}

	// Idempotency: a bundle for this exact content hash already cached?
	if existing, ok := s.findCachedUploadBundle(ctx, item.ID, zipSHA); ok {
		logger.Info("[bundle-pack] upload item %s already packed at %s, reusing artifact %s", item.ID, zipSHA, existing.ID)
		return existing, nil
	}

	repoID := s.resolveRepoID(item)
	storageKey := uploadBundleStorageKey(repoID, item.ID, zipSHA)

	artifact, err := s.storeBundleArtifact(ctx, storeBundleArtifactInput{
		item:       item,
		zipBytes:   zipBytes,
		zipSHA:     zipSHA,
		version:    zipSHA,
		storageKey: storageKey,
		sourceType: uploadBundleSourceType,
		uploadedBy: bundleArtifactUploader,
	})
	if err != nil {
		return nil, fmt.Errorf("upload bundle: %w", err)
	}

	logger.Info("[bundle-pack] packed upload item %s slug=%s hash=%s size=%d bytes", item.ID, item.Slug, zipSHA, len(zipBytes))
	return artifact, nil
}

// packAssetsZip writes the item's assets (text inline / binary from storage) into a
// deterministic ZIP and returns the bytes and their sha256 hex. Mirrors the
// reconstruction in DownloadPluginZip (text vs StorageKey, plus the item.Content
// fallback for an uncovered SourcePath) but emits in sorted order with a fixed
// modtime so the same inputs always hash identically (so csc caches by version key).
func (s *BundlePackService) packAssetsZip(ctx context.Context, item *models.CapabilityItem, assets []models.CapabilityAsset) ([]byte, string, error) {
	// Collect (relPath -> content provider) so we can emit deterministically.
	type fileEntry struct {
		rel  string
		mode bool // true => fetch from storage; false => inline text
		text []byte
		key  string
	}
	entries := make([]fileEntry, 0, len(assets)+1)
	sourcePathCovered := false
	for _, a := range assets {
		if a.RelPath == "" {
			continue
		}
		if a.RelPath == item.SourcePath {
			sourcePathCovered = true
		}
		if a.TextContent != nil {
			entries = append(entries, fileEntry{rel: a.RelPath, text: []byte(*a.TextContent)})
		} else if a.StorageKey != "" {
			entries = append(entries, fileEntry{rel: a.RelPath, mode: true, key: a.StorageKey})
		}
	}
	// Fallback: write the synthetic main file if it wasn't a stored asset.
	if !sourcePathCovered && item.Content != "" && item.SourcePath != "" {
		entries = append(entries, fileEntry{rel: item.SourcePath, text: []byte(item.Content)})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })

	fixedTime := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.rel, Method: zip.Deflate, Modified: fixedTime}
		w, createErr := zw.CreateHeader(hdr)
		if createErr != nil {
			zw.Close()
			return nil, "", fmt.Errorf("create zip entry %q: %w", e.rel, createErr)
		}
		if e.mode {
			reader, _, getErr := s.Storage.Get(ctx, e.key)
			if getErr != nil {
				zw.Close()
				return nil, "", fmt.Errorf("read stored asset %q: %w", e.key, getErr)
			}
			if _, copyErr := io.Copy(w, reader); copyErr != nil {
				reader.Close()
				zw.Close()
				return nil, "", fmt.Errorf("copy stored asset %q: %w", e.key, copyErr)
			}
			reader.Close()
		} else if _, wErr := w.Write(e.text); wErr != nil {
			zw.Close()
			return nil, "", fmt.Errorf("write text asset %q: %w", e.rel, wErr)
		}
	}
	if closeErr := zw.Close(); closeErr != nil {
		return nil, "", fmt.Errorf("finalize zip: %w", closeErr)
	}

	data := buf.Bytes()
	sum := sha256.Sum256(data)
	return data, fmt.Sprintf("%x", sum), nil
}

// findCachedUploadBundle returns the existing upload_pack artifact for the given
// content hash when its stored file is still present.
func (s *BundlePackService) findCachedUploadBundle(ctx context.Context, itemID, contentHash string) (*models.CapabilityArtifact, bool) {
	var existing models.CapabilityArtifact
	err := s.DB.WithContext(ctx).
		Where("item_id = ? AND source_type = ? AND artifact_version = ?", itemID, uploadBundleSourceType, contentHash).
		Order("created_at DESC").
		First(&existing).Error
	if err != nil {
		return nil, false
	}
	present, existsErr := s.Storage.Exists(ctx, existing.StorageKey)
	if existsErr != nil || !present {
		return nil, false
	}
	return &existing, true
}

// uploadBundleStorageKey builds the storage key for an uploaded-plugin bundle:
//
//	<repoID>/<itemID>/bundle/upload-<contentHash>.zip
func uploadBundleStorageKey(repoID, itemID, contentHash string) string {
	if repoID == "" {
		repoID = "public"
	}
	return fmt.Sprintf("%s/%s/bundle/upload-%s.zip", repoID, itemID, contentHash)
}

// SeedItemBundle writes a pre-packed plugin ZIP (carried inside an offline / air-gap
// catalog bundle) straight into storage + a `seeded` CapabilityArtifact, SKIPPING the
// lazy clone-and-pack. It is the air-gap counterpart of PackItemBundle: the bytes and
// the version key come from the offline bundle rather than from a server-side clone.
//
//   - version MUST be the whole-bundle truth (upstream commit SHA or content hash),
//     never the catalog semver / item.SourceSHA, so csc caches deterministically by
//     version key (design ADR-3).
//   - It is idempotent on (item, version): if a `seeded` artifact already exists for
//     this version and its stored file is present, the existing artifact is returned
//     without re-writing. This makes re-importing the same offline bundle a no-op.
//   - On success the produced artifact is IsLatest=true and any older `seeded`
//     artifact for the same item is demoted.
//
// The zip bytes are stored verbatim — the offline-bake side is responsible for
// producing a lossless ZIP (equivalent to a git clone). zipSHA, when non-empty, was
// already verified by the caller against the declared sha256; SeedItemBundle records
// it as the artifact checksum (computing it here when empty).
func (s *BundlePackService) SeedItemBundle(ctx context.Context, item *models.CapabilityItem, zipBytes []byte, version, zipSHA string) (*models.CapabilityArtifact, error) {
	if item == nil {
		return nil, fmt.Errorf("seed bundle: item is nil")
	}
	if version == "" {
		return nil, fmt.Errorf("seed bundle: item %s missing bundle version key", item.ID)
	}
	if len(zipBytes) == 0 {
		return nil, fmt.Errorf("seed bundle: item %s has empty bundle zip", item.ID)
	}
	if zipSHA == "" {
		sum := sha256.Sum256(zipBytes)
		zipSHA = fmt.Sprintf("%x", sum)
	}

	// Idempotency: a seeded bundle for this exact version already cached?
	if existing, ok := s.findCachedSeedBundle(ctx, item.ID, version); ok {
		logger.Info("[bundle-pack] seed item %s already seeded at %s, reusing artifact %s", item.ID, version, existing.ID)
		return existing, nil
	}

	repoID := s.resolveRepoID(item)
	storageKey := bundleStorageKey(repoID, item.ID, version)

	artifact, err := s.storeBundleArtifact(ctx, storeBundleArtifactInput{
		item:       item,
		zipBytes:   zipBytes,
		zipSHA:     zipSHA,
		version:    version,
		storageKey: storageKey,
		sourceType: BundleSourceTypeSeeded,
		uploadedBy: seedBundleUploader,
	})
	if err != nil {
		return nil, fmt.Errorf("seed bundle: %w", err)
	}

	logger.Info("[bundle-pack] seeded item %s slug=%s version=%s size=%d bytes", item.ID, item.Slug, version, len(zipBytes))
	return artifact, nil
}

// findCachedSeedBundle returns the existing seeded artifact for the given version
// when its stored file is still present.
func (s *BundlePackService) findCachedSeedBundle(ctx context.Context, itemID, version string) (*models.CapabilityArtifact, bool) {
	var existing models.CapabilityArtifact
	err := s.DB.WithContext(ctx).
		Where("item_id = ? AND source_type = ? AND artifact_version = ?", itemID, BundleSourceTypeSeeded, version).
		Order("created_at DESC").
		First(&existing).Error
	if err != nil {
		return nil, false
	}
	present, existsErr := s.Storage.Exists(ctx, existing.StorageKey)
	if existsErr != nil || !present {
		return nil, false
	}
	return &existing, true
}
