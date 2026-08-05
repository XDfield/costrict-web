// Package services — read-through content access for Git-backed capabilities.
//
// A Git-backed capability_items row is an index entry, not a copy: its content
// truth is the file at source_repo_path in the bound repository. This service
// is the only reader of that truth, and it reads it on every request.
//
// Why no cache (V4 §17 decision 18, S2 R2.9): a cache hit would simultaneously
// break "always live" and "unreachable means error" — the two properties that
// make read-through worth doing. A stale hit is indistinguishable from a fresh
// one, so the DB column that used to hold content would simply be replaced by a
// second copy with the same failure mode. Request volume is measured in the S7
// end-to-end pass; if it warrants caching, that arrives as its own change with
// an explicit key, invalidation, and unreachable-behaviour contract.
//
// Why the repository is located by numeric id rather than by parsing
// source_repo_url: the URL carries the owner and name as they were at bind
// time, and both change on a rename or a transfer. (git_server_id, repo_id) is
// the identity the whole Git-sync path is keyed on — the same pair the
// uq_capability_items_git_manifest index uses — so it is also the pair used
// here. The current owner/name come back from the Git server.
package services

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// gitCapabilityAssetHashWorkers bounds the fan-out of the asset digest pass.
//
// The digest cannot come from the tree listing: Gitea reports each entry's git
// blob id, which is sha1("blob <len>\0" + content) and therefore not the
// SHA-256 the assets contract carries. So every asset has to be read once, and
// reading them one after another would turn a listing of a large capability
// into N sequential round trips to the Git server. A small fixed fan-out keeps
// the latency proportional to N/workers without letting one request open an
// unbounded number of connections to a shared Git server.
const gitCapabilityAssetHashWorkers = 8

// Sentinel errors. Callers map them to HTTP status codes; none of them may be
// answered with the row's stored content. A Git-backed row's `content` column
// is residue from before the read-through existed (and is empty on every row
// discovery has created since), so falling back to it would serve data that is
// arbitrarily old while looking like success — the failure mode this whole
// change removes.
var (
	// ErrGitContentCoordinate — the row cannot name a file to read: missing
	// server id, missing/zero repo id, empty or unsafe manifest path. An
	// operator has to repair the row (or re-run discovery); no retry helps.
	ErrGitContentCoordinate = errors.New("services: git-backed item has no usable repository coordinate")

	// ErrGitContentServer — the git server this row points at is unknown,
	// disabled, or misconfigured. Configuration problem, not transient.
	ErrGitContentServer = errors.New("services: git server for this item is unavailable")

	// ErrGitContentUnreachable — transport failure, timeout, or an unexpected
	// status from the git server. Retryable.
	ErrGitContentUnreachable = errors.New("services: git server is unreachable")

	// ErrGitContentForbidden — the git server refused the read (401/403). With
	// the admin token this means the token lost its scope, not that the caller
	// lacks permission.
	ErrGitContentForbidden = errors.New("services: git server rejected the content read")

	// ErrGitContentMissing — the repository or the file is gone at the
	// requested ref. The index row outlived its source.
	ErrGitContentMissing = errors.New("services: file no longer exists in the git repository")
)

// GitContentServerResolver is deliberately narrower than gitserver.Resolver:
// a capability row names its git server directly, so there is no tenant to
// resolve through.
type GitContentServerResolver interface {
	ResolveByServerID(ctx context.Context, serverID string) (*gitserver.Config, error)
}

// GitCapabilityContentReader is the Git surface a content read needs. Both
// calls are reads; nothing on this path writes to the repository or the DB.
type GitCapabilityContentReader interface {
	GetRepoByID(ctx context.Context, repoID int64) (*gitsync.Repo, error)
	ListTree(ctx context.Context, owner, repo, ref string) ([]gitsync.GitTreeEntry, error)
	ReadRawFile(ctx context.Context, owner, repo, ref, filePath string) ([]byte, error)
}

// GitCapabilityAsset is a repository-backed file belonging to one capability.
// RelPath is relative to the capability root, so callers can install it without
// exposing the repository's internal prefix (for example plugins/foo/).
//
// ContentSHA is the SHA-256 of the file's bytes — the same digest DB-backed
// assets store in capability_assets.content_sha, because the two reach the
// device through one field of one response and the device applies one algorithm
// to it. It is deliberately NOT the git blob id the tree listing hands out:
// that is a SHA-1 over "blob <len>\0"+content, so passing it through would have
// made every verification fail while looking like the protection was on.
type GitCapabilityAsset struct {
	RelPath    string
	MimeType   string
	FileSize   int64
	ContentSHA string
}

type gitCapabilityContentTarget struct {
	reader       GitCapabilityContentReader
	owner        string
	repo         string
	ref          string
	manifestPath string
}

// GitCapabilityContentService serves capability content straight from Git.
type GitCapabilityContentService struct {
	Resolver GitContentServerResolver
	// NewReader builds the Git client for a resolved server. Nil uses
	// gitsync.NewClient, which is the same outbound client every other Git call
	// on the server goes through.
	NewReader func(*gitserver.Config) GitCapabilityContentReader
}

// NewGitCapabilityContentService binds the service to the git_servers table
// reachable through db.
func NewGitCapabilityContentService(db *gorm.DB) *GitCapabilityContentService {
	if db == nil {
		return nil
	}
	return &GitCapabilityContentService{Resolver: gitserver.NewDBResolver(db)}
}

// ItemContent returns the item's content exactly as the repository stores it —
// including any frontmatter.
//
// Frontmatter is not stripped, and callers must not strip it either. The device
// client writes this payload to SKILL.md byte for byte, and its loader reads
// description / disable-model-invocation / allowed-tools back out of that
// frontmatter. Stripping does not fail loudly: the skill installs, the loader
// finds no description, and the model is never offered the skill. A view that
// wants only the body derives it from this payload rather than changing what
// `content` means.
func (s *GitCapabilityContentService) ItemContent(ctx context.Context, item *models.CapabilityItem) (string, error) {
	raw, err := s.ItemContentBytes(ctx, item)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ItemContentBytes is ItemContent without the string conversion, for callers
// that stream the payload (the download endpoints).
func (s *GitCapabilityContentService) ItemContentBytes(ctx context.Context, item *models.CapabilityItem) ([]byte, error) {
	target, err := s.resolveTarget(ctx, item)
	if err != nil {
		return nil, err
	}

	raw, err := target.reader.ReadRawFile(ctx, target.owner, target.repo, target.ref, target.manifestPath)
	if err != nil {
		return nil, classifyGitContentError(err)
	}
	return raw, nil
}

// ItemAssets lists the live repository files owned by item, excluding the
// capability manifest itself and other recognizable capability manifests, and
// digests each one.
//
// The digests cost one extra read per asset. That is the price of the manifest
// being verifiable at all: the device compares sha256(content) against
// ContentSHA and skips the comparison entirely when the field is empty, so
// shipping the listing without digests is indistinguishable from shipping no
// integrity check. Callers that only need membership use target.assets directly
// and pay nothing.
func (s *GitCapabilityContentService) ItemAssets(ctx context.Context, item *models.CapabilityItem) ([]GitCapabilityAsset, error) {
	target, err := s.resolveTarget(ctx, item)
	if err != nil {
		return nil, err
	}
	assets, err := target.assets(ctx)
	if err != nil {
		return nil, err
	}
	if err := target.digestAssets(ctx, assets); err != nil {
		return nil, err
	}
	return assets, nil
}

// ItemAssetBytes reads a live asset after proving that it belongs to the
// capability's current tree. This prevents the registry route from becoming an
// arbitrary repository-file proxy.
func (s *GitCapabilityContentService) ItemAssetBytes(
	ctx context.Context, item *models.CapabilityItem, relPath string,
) ([]byte, GitCapabilityAsset, error) {
	target, err := s.resolveTarget(ctx, item)
	if err != nil {
		return nil, GitCapabilityAsset{}, err
	}
	normalized, err := NormalizeArchivePath(relPath)
	if err != nil || normalized != relPath {
		return nil, GitCapabilityAsset{}, fmt.Errorf("%w: invalid asset path %q", ErrGitContentCoordinate, relPath)
	}
	assets, err := target.assets(ctx)
	if err != nil {
		return nil, GitCapabilityAsset{}, err
	}
	for _, asset := range assets {
		if asset.RelPath != normalized {
			continue
		}
		raw, readErr := target.readAsset(ctx, normalized)
		if readErr != nil {
			return nil, GitCapabilityAsset{}, readErr
		}
		// The bytes are in hand, so the digest is free here — no reason to hand
		// back the one asset shape in this package that carries an empty one.
		asset.ContentSHA = sha256Hex(raw)
		return raw, asset, nil
	}
	return nil, GitCapabilityAsset{}, fmt.Errorf("%w: asset %q is not in the capability tree", ErrGitContentMissing, relPath)
}

func (s *GitCapabilityContentService) resolveTarget(
	ctx context.Context, item *models.CapabilityItem,
) (*gitCapabilityContentTarget, error) {
	if s == nil || s.Resolver == nil {
		return nil, fmt.Errorf("%w: read-through is not configured", ErrGitContentServer)
	}
	if item == nil || item.ContentBackend != models.ContentBackendGit {
		return nil, fmt.Errorf("%w: item is not git-backed", ErrGitContentCoordinate)
	}

	serverID := strings.TrimSpace(item.SourceGitServerID)
	filePath := strings.TrimSpace(item.SourceRepoPath)
	if serverID == "" || item.SourceGitRepoID <= 0 || filePath == "" {
		return nil, fmt.Errorf("%w: server=%q repo=%d path=%q",
			ErrGitContentCoordinate, serverID, item.SourceGitRepoID, filePath)
	}
	// The path comes out of the database, so it is validated on the way out as
	// well as on the way in: a row written before the sync path validated paths
	// (or edited by hand) must not be able to aim the read at ../.
	if err := validateGitManifestPath(filePath); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGitContentCoordinate, err)
	}
	ref := gitCapabilityContentRef(item)
	if ref == "" {
		return nil, fmt.Errorf("%w: item has neither a branch nor a commit to read from", ErrGitContentCoordinate)
	}

	cfg, err := s.Resolver.ResolveByServerID(ctx, serverID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGitContentServer, err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("%w: server %q resolved to no configuration", ErrGitContentServer, serverID)
	}
	reader := s.reader(cfg)
	if reader == nil {
		return nil, fmt.Errorf("%w: server %q has no usable client", ErrGitContentServer, serverID)
	}

	repo, err := reader.GetRepoByID(ctx, item.SourceGitRepoID)
	if err != nil {
		return nil, classifyGitContentError(err)
	}
	if repo == nil {
		return nil, fmt.Errorf("%w: repository %d is gone", ErrGitContentMissing, item.SourceGitRepoID)
	}
	owner, name, err := splitGitRepoFullName(repo.FullName)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGitContentUnreachable, err)
	}

	return &gitCapabilityContentTarget{
		reader: reader, owner: owner, repo: name, ref: ref, manifestPath: filePath,
	}, nil
}

func (t *gitCapabilityContentTarget) assets(ctx context.Context) ([]GitCapabilityAsset, error) {
	tree, err := t.reader.ListTree(ctx, t.owner, t.repo, t.ref)
	if err != nil {
		return nil, classifyGitContentError(err)
	}
	root := capabilityAssetRoot(t.manifestPath)
	manifestFound := false
	assets := make([]GitCapabilityAsset, 0)
	for _, entry := range tree {
		if entry.Type != "" && !strings.EqualFold(entry.Type, "blob") {
			continue
		}
		normalized, normalizeErr := NormalizeArchivePath(entry.Path)
		if normalizeErr != nil || normalized != entry.Path {
			return nil, fmt.Errorf("%w: invalid repository path %q", ErrGitContentCoordinate, entry.Path)
		}
		if normalized == t.manifestPath {
			manifestFound = true
			continue
		}
		// Platform bookkeeping is not capability content. The ownership marker is
		// written by provisioning at the REPOSITORY root, so it is excluded before
		// the capability root is stripped; the second check below covers a marker
		// sitting at the capability root of a monorepo. Without this the device
		// installs .costrict/capability.json alongside the skill, and the user
		// receives a file describing our provisioning bookkeeping.
		if normalized == GitCapabilityOwnershipMarkerPath {
			continue
		}
		if root != "" {
			prefix := root + "/"
			if !strings.HasPrefix(normalized, prefix) {
				continue
			}
			normalized = strings.TrimPrefix(normalized, prefix)
			if normalized == GitCapabilityOwnershipMarkerPath {
				continue
			}
		}
		// A multi-capability repository must not install another item's manifest
		// as an attachment of this item.
		if IsGitCapabilityManifestPath(entry.Path) {
			continue
		}
		assets = append(assets, GitCapabilityAsset{
			RelPath: normalized, MimeType: InferMimeType(normalized), FileSize: entry.Size,
		})
	}
	if !manifestFound {
		return nil, fmt.Errorf("%w: manifest %q is not in the repository tree", ErrGitContentMissing, t.manifestPath)
	}
	return assets, nil
}

// readAsset reads one asset by its capability-relative path. The caller must
// have proved the path is in the capability tree first; this only re-attaches
// the repository prefix the listing stripped.
func (t *gitCapabilityContentTarget) readAsset(ctx context.Context, relPath string) ([]byte, error) {
	repoPath := relPath
	if root := capabilityAssetRoot(t.manifestPath); root != "" {
		repoPath = path.Join(root, relPath)
	}
	raw, err := t.reader.ReadRawFile(ctx, t.owner, t.repo, t.ref, repoPath)
	if err != nil {
		return nil, classifyGitContentError(err)
	}
	return raw, nil
}

// digestAssets fills in every asset's ContentSHA in place.
//
// A failure fails the whole listing rather than leaving that asset's digest
// empty: an empty digest is silently skipped by the device, so degrading here
// would reintroduce exactly the unverified manifest this pass exists to remove.
// The first error wins and cancels the rest.
func (t *gitCapabilityContentTarget) digestAssets(ctx context.Context, assets []GitCapabilityAsset) error {
	if len(assets) == 0 {
		return nil
	}
	workers := gitCapabilityAssetHashWorkers
	if len(assets) < workers {
		workers = len(assets)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	indexes := make(chan int)
	failures := make(chan error, workers)
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			for index := range indexes {
				raw, err := t.readAsset(ctx, assets[index].RelPath)
				if err != nil {
					failures <- err
					cancel()
					return
				}
				// Distinct indexes, so the writes do not overlap.
				assets[index].ContentSHA = sha256Hex(raw)
			}
		}()
	}
	go func() {
		defer close(indexes)
		for index := range assets {
			select {
			case indexes <- index:
			case <-ctx.Done():
				return
			}
		}
	}()
	wg.Wait()
	close(failures)
	return <-failures
}

func capabilityAssetRoot(manifestPath string) string {
	root := path.Dir(manifestPath)
	if root == "." || root == "/" {
		return ""
	}
	return root
}

// gitCapabilityContentRef picks the ref to read at.
//
// The branch wins over git_sha on purpose. git_sha is the commit the last
// successful sync projected metadata from, so pinning content to it would make
// the content only as fresh as the last webhook — the same staleness the DB
// column had, just relocated. Reading the branch means a push is visible on the
// next request whether or not its sync job has run yet; the metadata catches up
// when the job lands. git_sha is the fallback for a row whose branch was never
// recorded.
func gitCapabilityContentRef(item *models.CapabilityItem) string {
	if ref := strings.TrimSpace(item.SourceRepoRef); ref != "" {
		return ref
	}
	return strings.TrimSpace(item.GitSHA)
}

func (s *GitCapabilityContentService) reader(cfg *gitserver.Config) GitCapabilityContentReader {
	if s.NewReader != nil {
		return s.NewReader(cfg)
	}
	client := gitsync.NewClient(cfg.Endpoint, cfg.AdminToken)
	if client == nil {
		return nil
	}
	return client
}

// classifyGitContentError maps the Git client's sentinels onto this package's,
// so callers switch on one vocabulary. An unrecognised error is treated as
// unreachable: the safe reading is "the upstream did not answer usefully", and
// every branch here ends in an error rather than in stored content.
func classifyGitContentError(err error) error {
	switch {
	case errors.Is(err, gitsync.ErrGiteaTeamNotFound):
		return fmt.Errorf("%w: %v", ErrGitContentMissing, err)
	case errors.Is(err, gitsync.ErrGiteaUnauthorized):
		return fmt.Errorf("%w: %v", ErrGitContentForbidden, err)
	case errors.Is(err, gitsync.ErrGiteaTimeout):
		return fmt.Errorf("%w: %v", ErrGitContentUnreachable, err)
	default:
		return fmt.Errorf("%w: %v", ErrGitContentUnreachable, err)
	}
}
