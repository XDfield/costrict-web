// Real-Gitea verification of the provisioning call sequence.
//
// Everything else in this package fakes the Gitea edge with httptest, which
// pins OUR contract but cannot tell whether Gitea actually behaves that way.
// Four assumptions in provisionGitCapabilityRepo are only true if the server
// agrees, and each has a plausible failure mode:
//
//   - CreateRepo goes through /admin/users/{owner}/repos, which is the only
//     call that can place a repository in someone else's namespace;
//   - a user PAT minted with write:repository can write into that repository,
//     even though the repository was created by the administrator;
//   - the contents API round-trips the manifest byte for byte (no BOM, no
//     newline normalisation) — content_md5 is computed over those bytes;
//   - ListTree answers for a freshly auto-initialised repository, which is what
//     the "is this repository already someone else's capability" guard reads.
//
// Skipped unless GITEA_E2E_URL is set, so the ordinary suite stays hermetic.
// Run against the local instance with:
//
//	GITEA_E2E_URL=http://localhost:3001 \
//	GITEA_E2E_ADMIN_TOKEN=<token> GITEA_E2E_ADMIN_USER=gitadmin \
//	GITEA_E2E_ADMIN_PASSWORD=<password> \
//	go test ./internal/handlers -run TestProvisionSequenceAgainstRealGitea -v

package handlers

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/services"
)

func TestProvisionSequenceAgainstRealGitea(t *testing.T) {
	endpoint := strings.TrimSpace(os.Getenv("GITEA_E2E_URL"))
	adminToken := strings.TrimSpace(os.Getenv("GITEA_E2E_ADMIN_TOKEN"))
	adminUser := strings.TrimSpace(os.Getenv("GITEA_E2E_ADMIN_USER"))
	adminPassword := strings.TrimSpace(os.Getenv("GITEA_E2E_ADMIN_PASSWORD"))
	if endpoint == "" || adminToken == "" || adminUser == "" || adminPassword == "" {
		t.Skip("set GITEA_E2E_URL / GITEA_E2E_ADMIN_TOKEN / GITEA_E2E_ADMIN_USER / GITEA_E2E_ADMIN_PASSWORD to run")
	}

	ctx := context.Background()
	admin := gitsync.NewClient(endpoint, adminToken)
	if admin == nil {
		t.Fatal("admin client unavailable")
	}

	// A stand-in for the cs-user short_id the binding carries. Suffixed with a
	// timestamp so repeated runs never collide on a stale account.
	owner := fmt.Sprintf("s3u%d", time.Now().Unix()%100000)
	if _, err := admin.CreateUser(ctx, gitsync.CreateUserOptions{
		Login:    owner,
		Email:    owner + "@costrict.internal",
		FullName: "S3 provisioning probe",
		Password: "S3Probe@" + owner,
	}); err != nil {
		t.Fatalf("create probe user %s: %v", owner, err)
	}

	basic := gitsync.NewClientWithBasicAuth(endpoint, adminToken, adminUser, adminPassword)
	tok, err := basic.CreateUserToken(ctx, owner, gitsync.CreateUserTokenOptions{
		Name:   "s3-provision-probe",
		Scopes: []string{"write:repository", "read:user"},
	})
	if err != nil {
		t.Fatalf("mint PAT for %s: %v", owner, err)
	}
	user := gitsync.NewUserClient(endpoint, tok.TokenPlaintext)

	for _, itemType := range []string{"skill", "subagent", "command", "mcp"} {
		t.Run(itemType, func(t *testing.T) {
			manifestPath, ok := gitCapabilityManifestPath(itemType)
			if !ok {
				t.Fatalf("no manifest path for %s", itemType)
			}
			slug := "probe-" + itemType
			content, err := buildGitCapabilityManifest(gitCapabilityProvisionSpec{
				ItemType: itemType, Slug: slug, Name: "Probe " + itemType,
				Description: "provisioning probe", Category: "utilities", Version: "1.0.0",
				Tags: []string{"probe"}, Author: "costrict", License: "MIT",
			})
			if err != nil {
				t.Fatalf("build skeleton: %v", err)
			}

			repo, err := admin.CreateRepo(ctx, owner, gitsync.CreateRepoOptions{
				Name: slug, Description: "probe", Private: false,
				AutoInit: true, DefaultBranch: gitCapabilityRepoBranch,
			})
			if err != nil {
				t.Fatalf("create repo %s/%s: %v", owner, slug, err)
			}
			t.Cleanup(func() {
				_, _ = admin.GetRepo(ctx, owner, slug) // best effort; repos are left for inspection
			})
			if repo.ID <= 0 {
				t.Fatalf("gitea returned no stable repo id: %+v", repo)
			}
			branch := firstNonEmpty(repo.DefaultBranch, gitCapabilityRepoBranch)

			// The adoption guard reads the tree of a repository that already
			// exists. On a fresh auto-init repo it must answer, not error.
			tree, err := admin.ListTree(ctx, owner, slug, branch)
			if err != nil {
				t.Fatalf("list tree of a fresh repo: %v", err)
			}
			for _, entry := range tree {
				if services.IsGitCapabilityManifestPath(entry.Path) {
					t.Fatalf("auto-init left a capability manifest in the tree: %s", entry.Path)
				}
			}

			// The write is signed with the USER's PAT even though the admin
			// created the repository.
			if err := user.WriteFile(ctx, owner, slug, branch, manifestPath, content,
				"feat("+slug+"): publish capability manifest"); err != nil {
				t.Fatalf("write %s with the user PAT: %v", manifestPath, err)
			}

			stored, err := user.ReadFile(ctx, owner, slug, branch, manifestPath)
			if err != nil {
				t.Fatalf("read back %s: %v", manifestPath, err)
			}
			if !bytes.Equal(stored, content) {
				t.Fatalf("manifest did not round-trip byte for byte:\n got %q\nwant %q", stored, content)
			}

			// And the repository now describes itself: discovery classifies the
			// file as this type and parses one capability out of it.
			candidateType, classified := services.ClassifyGitCapabilityManifestType(manifestPath)
			if !classified || candidateType != itemType {
				t.Fatalf("discovery classifies %q as (%q,%v), want %q", manifestPath, candidateType, classified, itemType)
			}
			parsed, err := (&services.ParserService{}).ParseGitDiscoveryFile(stored, manifestPath, itemType)
			if err != nil {
				t.Fatalf("discovery cannot parse the provisioned manifest: %v", err)
			}
			if len(parsed) != 1 {
				t.Fatalf("provisioned manifest must describe exactly one capability, got %d", len(parsed))
			}
			t.Logf("provisioned %s/%s id=%d manifest=%s capability=%q",
				owner, slug, repo.ID, manifestPath, parsed[0].Name)
		})
	}
}
