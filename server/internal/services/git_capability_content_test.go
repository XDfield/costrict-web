package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
)

type stubContentResolver struct {
	cfg *gitserver.Config
	err error
}

func (s stubContentResolver) ResolveByServerID(context.Context, string) (*gitserver.Config, error) {
	return s.cfg, s.err
}

type stubContentReader struct {
	repo    *gitsync.Repo
	repoErr error
	tree    []gitsync.GitTreeEntry
	treeErr error

	file    string
	fileErr error

	reads []string
}

func (s *stubContentReader) GetRepoByID(context.Context, int64) (*gitsync.Repo, error) {
	return s.repo, s.repoErr
}

func (s *stubContentReader) ListTree(context.Context, string, string, string) ([]gitsync.GitTreeEntry, error) {
	return s.tree, s.treeErr
}

func (s *stubContentReader) ReadRawFile(_ context.Context, owner, repo, ref, filePath string) ([]byte, error) {
	s.reads = append(s.reads, owner+"/"+repo+"@"+ref+":"+filePath)
	if s.fileErr != nil {
		return nil, s.fileErr
	}
	return []byte(s.file), nil
}

func newContentSvc(reader *stubContentReader) *GitCapabilityContentService {
	return &GitCapabilityContentService{
		Resolver: stubContentResolver{cfg: &gitserver.Config{ServerID: "gs-1", Endpoint: "http://git.test", AdminToken: "t"}},
		NewReader: func(*gitserver.Config) GitCapabilityContentReader {
			return reader
		},
	}
}

func gitContentItem() *models.CapabilityItem {
	return &models.CapabilityItem{
		ID: "item-1", ItemType: "skill", ContentBackend: models.ContentBackendGit,
		Content: "STALE", SourceGitServerID: "gs-1", SourceGitRepoID: 7,
		SourceRepoPath: "skills/demo/skill.md", SourceRepoRef: "main",
		GitSHA: strings.Repeat("b", 40),
	}
}

// The current owner/name come from the numeric repository id, never from the
// stored URL: a rename leaves that URL pointing at nothing.
func TestGitCapabilityContent_ResolvesRepositoryByNumericIdentity(t *testing.T) {
	reader := &stubContentReader{
		repo: &gitsync.Repo{ID: 7, FullName: "renamed-owner/renamed-repo"},
		file: "---\nname: demo\n---\nbody",
	}
	item := gitContentItem()
	item.SourceRepoURL = "http://git.test/old-owner/old-repo"

	got, err := newContentSvc(reader).ItemContent(context.Background(), item)
	if err != nil {
		t.Fatalf("ItemContent: %v", err)
	}
	if got != reader.file {
		t.Fatalf("content = %q", got)
	}
	if len(reader.reads) != 1 || reader.reads[0] != "renamed-owner/renamed-repo@main:skills/demo/skill.md" {
		t.Fatalf("unexpected reads: %v", reader.reads)
	}
}

func TestGitCapabilityContent_ListsAndReadsAssetsWithinCapabilityRoot(t *testing.T) {
	reader := &stubContentReader{
		repo: &gitsync.Repo{ID: 7, FullName: "owner/repo"},
		tree: []gitsync.GitTreeEntry{
			{Path: "skills/demo/SKILL.md", Type: "blob", Size: 10},
			{Path: "skills/demo/assets/logo.png", Type: "blob", Size: 4},
			{Path: "skills/other/SKILL.md", Type: "blob", Size: 12},
		},
		file: "logo",
	}
	item := gitContentItem()
	item.SourceRepoPath = "skills/demo/SKILL.md"

	assets, err := newContentSvc(reader).ItemAssets(context.Background(), item)
	if err != nil {
		t.Fatalf("ItemAssets: %v", err)
	}
	if len(assets) != 1 || assets[0].RelPath != "assets/logo.png" || assets[0].FileSize != 4 {
		t.Fatalf("unexpected assets: %#v", assets)
	}
	raw, asset, err := newContentSvc(reader).ItemAssetBytes(context.Background(), item, "assets/logo.png")
	if err != nil {
		t.Fatalf("ItemAssetBytes: %v", err)
	}
	if string(raw) != "logo" || asset.RelPath != "assets/logo.png" {
		t.Fatalf("unexpected asset read: %q %#v", raw, asset)
	}
	if got := reader.reads[len(reader.reads)-1]; got != "owner/repo@main:skills/demo/assets/logo.png" {
		t.Fatalf("asset read used wrong repository path: %s", got)
	}
}

// Provisioning bookkeeping is not capability content.
//
// The tree here deliberately carries the two files a provisioned repository
// really contains beyond the capability itself: the ownership marker this
// rollout added, and the README auto-init used to commit. Both used to be
// reported as assets, so a fork installed them onto the user's device.
//
// The assertion is on the WHOLE manifest, not on "the files I expected are
// present". Checking only expected entries is exactly how this shipped: the
// end-to-end pass compared four known files and never asked whether there was a
// fifth.
func TestGitCapabilityContent_AssetsExcludeProvisioningBookkeeping(t *testing.T) {
	reader := &stubContentReader{
		repo: &gitsync.Repo{ID: 7, FullName: "u-e2es7/fix-01-skill-single"},
		tree: []gitsync.GitTreeEntry{
			{Path: "skill.md", Type: "blob", Size: 220},
			{Path: GitCapabilityOwnershipMarkerPath, Type: "blob", Size: 96},
			{Path: "README.md", Type: "blob", Size: 135},
			{Path: "references/guide.md", Type: "blob", Size: 42},
		},
	}
	item := gitContentItem()
	item.SourceRepoPath = "skill.md"

	assets, err := newContentSvc(reader).ItemAssets(context.Background(), item)
	if err != nil {
		t.Fatalf("ItemAssets: %v", err)
	}
	got := make([]string, 0, len(assets))
	for _, asset := range assets {
		got = append(got, asset.RelPath)
	}
	// README.md stays: once the repository exists, a README is the author's file
	// and we cannot tell it from a generated one. The fix for the generated one
	// is at the source (provisioning no longer creates it), not a name filter
	// that would silently drop real documentation.
	want := []string{"README.md", "references/guide.md"}
	if len(got) != len(want) {
		t.Fatalf("asset manifest has %d entries %v, want exactly %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("asset manifest = %v, want %v", got, want)
		}
	}
	for _, relPath := range got {
		if relPath == GitCapabilityOwnershipMarkerPath {
			t.Fatalf("ownership marker leaked into the asset manifest: %v", got)
		}
	}

	// And it cannot be fetched through the asset proxy either: the byte reader
	// resolves against this same manifest, so an excluded path is unreadable
	// rather than merely unlisted.
	if _, _, err := newContentSvc(reader).ItemAssetBytes(
		context.Background(), item, GitCapabilityOwnershipMarkerPath,
	); !errors.Is(err, ErrGitContentMissing) {
		t.Fatalf("marker fetched through the asset proxy: err = %v, want ErrGitContentMissing", err)
	}
}

// Same exclusion one level down: a monorepo capability whose own root carries a
// marker must not install it either.
func TestGitCapabilityContent_AssetsExcludeMarkerAtCapabilityRoot(t *testing.T) {
	reader := &stubContentReader{
		repo: &gitsync.Repo{ID: 7, FullName: "owner/pack"},
		tree: []gitsync.GitTreeEntry{
			{Path: "skills/demo/SKILL.md", Type: "blob", Size: 10},
			{Path: "skills/demo/" + GitCapabilityOwnershipMarkerPath, Type: "blob", Size: 96},
			{Path: GitCapabilityOwnershipMarkerPath, Type: "blob", Size: 96},
			{Path: "skills/demo/assets/logo.png", Type: "blob", Size: 4},
		},
	}
	item := gitContentItem()
	item.SourceRepoPath = "skills/demo/SKILL.md"

	assets, err := newContentSvc(reader).ItemAssets(context.Background(), item)
	if err != nil {
		t.Fatalf("ItemAssets: %v", err)
	}
	if len(assets) != 1 || assets[0].RelPath != "assets/logo.png" {
		t.Fatalf("asset manifest = %#v, want exactly [assets/logo.png]", assets)
	}
}

// The branch is the ref, not the last synced commit: pinning to git_sha would
// make content only as fresh as the last webhook, which is the staleness this
// path exists to remove. git_sha is the fallback for rows without a branch.
func TestGitCapabilityContent_PrefersBranchAndFallsBackToCommit(t *testing.T) {
	reader := &stubContentReader{repo: &gitsync.Repo{ID: 7, FullName: "a/b"}, file: "x"}
	svc := newContentSvc(reader)

	item := gitContentItem()
	if _, err := svc.ItemContent(context.Background(), item); err != nil {
		t.Fatalf("branch read: %v", err)
	}
	item.SourceRepoRef = ""
	if _, err := svc.ItemContent(context.Background(), item); err != nil {
		t.Fatalf("commit read: %v", err)
	}
	if len(reader.reads) != 2 ||
		!strings.Contains(reader.reads[0], "@main:") ||
		!strings.Contains(reader.reads[1], "@"+strings.Repeat("b", 40)+":") {
		t.Fatalf("unexpected refs: %v", reader.reads)
	}
}

// Every failure is an error and none of them returns the stored column.
func TestGitCapabilityContent_FailuresNeverFallBackToStoredContent(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(item *models.CapabilityItem, reader *stubContentReader)
		want   error
	}{
		{"no server id", func(i *models.CapabilityItem, _ *stubContentReader) { i.SourceGitServerID = "" }, ErrGitContentCoordinate},
		{"no repo id", func(i *models.CapabilityItem, _ *stubContentReader) { i.SourceGitRepoID = 0 }, ErrGitContentCoordinate},
		{"no path", func(i *models.CapabilityItem, _ *stubContentReader) { i.SourceRepoPath = "" }, ErrGitContentCoordinate},
		{"traversal path", func(i *models.CapabilityItem, _ *stubContentReader) { i.SourceRepoPath = "../../etc/passwd" }, ErrGitContentCoordinate},
		{"no ref at all", func(i *models.CapabilityItem, _ *stubContentReader) { i.SourceRepoRef, i.GitSHA = "", "" }, ErrGitContentCoordinate},
		{"repository gone", func(_ *models.CapabilityItem, r *stubContentReader) { r.repo = nil }, ErrGitContentMissing},
		{"file gone", func(_ *models.CapabilityItem, r *stubContentReader) { r.fileErr = gitsync.ErrGiteaNotFound }, ErrGitContentMissing},
		{"unauthorized", func(_ *models.CapabilityItem, r *stubContentReader) { r.fileErr = gitsync.ErrGiteaUnauthorized }, ErrGitContentForbidden},
		{"unreachable", func(_ *models.CapabilityItem, r *stubContentReader) { r.fileErr = gitsync.ErrGiteaUnreachable }, ErrGitContentUnreachable},
		{"timeout", func(_ *models.CapabilityItem, r *stubContentReader) { r.fileErr = gitsync.ErrGiteaTimeout }, ErrGitContentUnreachable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reader := &stubContentReader{repo: &gitsync.Repo{ID: 7, FullName: "a/b"}, file: "fresh"}
			item := gitContentItem()
			tc.mutate(item, reader)

			got, err := newContentSvc(reader).ItemContent(context.Background(), item)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if got != "" {
				t.Fatalf("a failed read returned content: %q", got)
			}
		})
	}
}

func TestGitCapabilityContent_UnresolvableServerIsItsOwnFailure(t *testing.T) {
	svc := &GitCapabilityContentService{
		Resolver: stubContentResolver{err: gitserver.ErrGitServerNotFound},
	}
	if _, err := svc.ItemContent(context.Background(), gitContentItem()); !errors.Is(err, ErrGitContentServer) {
		t.Fatalf("error = %v, want ErrGitContentServer", err)
	}
}

// DB-backed rows have no business on this path at all.
func TestGitCapabilityContent_RefusesDBBackedItems(t *testing.T) {
	reader := &stubContentReader{repo: &gitsync.Repo{ID: 7, FullName: "a/b"}, file: "fresh"}
	item := gitContentItem()
	item.ContentBackend = models.ContentBackendDB

	if _, err := newContentSvc(reader).ItemContent(context.Background(), item); !errors.Is(err, ErrGitContentCoordinate) {
		t.Fatalf("error = %v, want ErrGitContentCoordinate", err)
	}
	if len(reader.reads) != 0 {
		t.Fatalf("a db-backed row reached the git server: %v", reader.reads)
	}
}
