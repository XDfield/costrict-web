package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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

// stubContentReader is read concurrently: the asset digest pass fans out across
// workers, so the call log needs its own lock even though nothing else here
// races.
type stubContentReader struct {
	repo    *gitsync.Repo
	repoErr error
	tree    []gitsync.GitTreeEntry
	treeErr error

	file string
	// files, when set for a repository path, overrides file for that path so a
	// test can give each asset distinct bytes (and therefore a distinct digest).
	files   map[string]string
	fileErr error

	mu    sync.Mutex
	reads []string
}

func (s *stubContentReader) GetRepoByID(context.Context, int64) (*gitsync.Repo, error) {
	return s.repo, s.repoErr
}

func (s *stubContentReader) ListTree(context.Context, string, string, string) ([]gitsync.GitTreeEntry, error) {
	return s.tree, s.treeErr
}

func (s *stubContentReader) ReadRawFile(_ context.Context, owner, repo, ref, filePath string) ([]byte, error) {
	s.mu.Lock()
	s.reads = append(s.reads, owner+"/"+repo+"@"+ref+":"+filePath)
	s.mu.Unlock()
	if s.fileErr != nil {
		return nil, s.fileErr
	}
	if body, ok := s.files[filePath]; ok {
		return []byte(body), nil
	}
	return []byte(s.file), nil
}

// readLog returns a copy of the call log, so an assertion cannot race the
// workers that may still be unwinding after a cancelled digest pass.
func (s *stubContentReader) readLog() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.reads))
	copy(out, s.reads)
	return out
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
	if reads := reader.readLog(); len(reads) != 1 || reads[0] != "renamed-owner/renamed-repo@main:skills/demo/skill.md" {
		t.Fatalf("unexpected reads: %v", reads)
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
	reads := reader.readLog()
	if got := reads[len(reads)-1]; got != "owner/repo@main:skills/demo/assets/logo.png" {
		t.Fatalf("asset read used wrong repository path: %s", got)
	}
}

// The asset manifest has to carry the SAME digest the device recomputes, which
// is SHA-256 over the file bytes.
//
// The manifest used to leave ContentSHA empty. That does not fail loudly: the
// device guards its comparison with `if (asset.contentSha)`, so an empty string
// skips verification entirely and the install succeeds having checked nothing.
// Nor can the digest be lifted from the tree listing — GitTreeEntry.SHA is the
// git blob id, sha1("blob <len>\0"+content), which would fail every comparison
// it was fed to. So the assertion here is against a digest computed from the
// bytes, per asset, with the entries deliberately given different content.
func TestGitCapabilityContent_AssetManifestCarriesContentSHA256(t *testing.T) {
	bodies := map[string]string{
		"skills/demo/README.md":           "readme body\n",
		"skills/demo/assets/logo.png":     "\x89PNG\r\n\x1a\nbinary-ish",
		"skills/demo/assets/nested/a.txt": "third file",
	}
	reader := &stubContentReader{
		repo: &gitsync.Repo{ID: 7, FullName: "owner/repo"},
		tree: []gitsync.GitTreeEntry{
			{Path: "skills/demo/SKILL.md", Type: "blob", Size: 10, SHA: strings.Repeat("f", 40)},
			{Path: "skills/demo/README.md", Type: "blob", Size: 12, SHA: strings.Repeat("e", 40)},
			{Path: "skills/demo/assets/logo.png", Type: "blob", Size: 20, SHA: strings.Repeat("d", 40)},
			{Path: "skills/demo/assets/nested/a.txt", Type: "blob", Size: 10, SHA: strings.Repeat("c", 40)},
		},
		files: bodies,
	}
	item := gitContentItem()
	item.SourceRepoPath = "skills/demo/SKILL.md"

	assets, err := newContentSvc(reader).ItemAssets(context.Background(), item)
	if err != nil {
		t.Fatalf("ItemAssets: %v", err)
	}
	if len(assets) != 3 {
		t.Fatalf("asset manifest = %#v, want 3 entries", assets)
	}
	seen := map[string]bool{}
	for _, asset := range assets {
		want := sha256Hex([]byte(bodies["skills/demo/"+asset.RelPath]))
		if asset.ContentSHA == "" {
			t.Fatalf("asset %q has an empty contentSha; the device skips verification on empty", asset.RelPath)
		}
		if asset.ContentSHA != want {
			t.Errorf("asset %q contentSha = %q, want sha256 of its bytes %q", asset.RelPath, asset.ContentSHA, want)
		}
		if len(asset.ContentSHA) != 64 {
			t.Errorf("asset %q contentSha %q is not a SHA-256 hex digest (a git blob id would be 40)", asset.RelPath, asset.ContentSHA)
		}
		if seen[asset.ContentSHA] {
			t.Errorf("two assets share digest %q, so the digest is not derived from their bytes", asset.ContentSHA)
		}
		seen[asset.ContentSHA] = true
	}

	// The single-asset path returns the same digest, computed from the bytes it
	// already holds.
	_, one, err := newContentSvc(reader).ItemAssetBytes(context.Background(), item, "assets/logo.png")
	if err != nil {
		t.Fatalf("ItemAssetBytes: %v", err)
	}
	if want := sha256Hex([]byte(bodies["skills/demo/assets/logo.png"])); one.ContentSHA != want {
		t.Errorf("single asset contentSha = %q, want %q", one.ContentSHA, want)
	}
}

// Digesting one asset costs one read; resolving one asset must not cost N. The
// membership check shares the listing, not the digest pass, so a device pulling
// a 40-file capability makes 40 reads and not 1600.
func TestGitCapabilityContent_AssetBytesDoesNotDigestTheWholeTree(t *testing.T) {
	tree := []gitsync.GitTreeEntry{{Path: "skill.md", Type: "blob", Size: 10}}
	for i := 0; i < 10; i++ {
		tree = append(tree, gitsync.GitTreeEntry{Path: fmt.Sprintf("assets/f%d.txt", i), Type: "blob", Size: 3})
	}
	reader := &stubContentReader{repo: &gitsync.Repo{ID: 7, FullName: "owner/repo"}, tree: tree, file: "abc"}
	item := gitContentItem()
	item.SourceRepoPath = "skill.md"

	if _, _, err := newContentSvc(reader).ItemAssetBytes(context.Background(), item, "assets/f3.txt"); err != nil {
		t.Fatalf("ItemAssetBytes: %v", err)
	}
	reads := reader.readLog()
	if len(reads) != 1 || !strings.HasSuffix(reads[0], ":assets/f3.txt") {
		t.Fatalf("reads = %v, want exactly the requested asset", reads)
	}
}

// A digest that cannot be computed fails the listing. Returning the entry with
// an empty digest instead would be indistinguishable, to the device, from "this
// file needs no verification".
func TestGitCapabilityContent_AssetManifestFailsWhenAnAssetCannotBeRead(t *testing.T) {
	reader := &stubContentReader{
		repo: &gitsync.Repo{ID: 7, FullName: "owner/repo"},
		tree: []gitsync.GitTreeEntry{
			{Path: "skill.md", Type: "blob", Size: 10},
			{Path: "assets/gone.txt", Type: "blob", Size: 3},
		},
		fileErr: gitsync.ErrGiteaTeamNotFound,
	}
	item := gitContentItem()
	item.SourceRepoPath = "skill.md"

	assets, err := newContentSvc(reader).ItemAssets(context.Background(), item)
	if !errors.Is(err, ErrGitContentMissing) {
		t.Fatalf("ItemAssets err = %v (assets %#v), want ErrGitContentMissing", err, assets)
	}
	if assets != nil {
		t.Fatalf("a failed digest pass returned a partial manifest: %#v", assets)
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
