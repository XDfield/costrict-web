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
	"strings"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

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
	ReadRawFile(ctx context.Context, owner, repo, ref, filePath string) ([]byte, error)
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

	raw, err := reader.ReadRawFile(ctx, owner, name, ref, filePath)
	if err != nil {
		return nil, classifyGitContentError(err)
	}
	return raw, nil
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
