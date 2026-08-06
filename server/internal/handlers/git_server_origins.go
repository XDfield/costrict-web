// Package handlers — the browser-reachable list of Git server origins.
//
// ── The gap this closes ─────────────────────────────────────────────────────
//
// A Git-backed item carries `sourceRepoUrl`, and the hub renders it as three
// controls a user clicks: "Edit in Git", "History", "Download .zip". The
// frontend validates that string before it becomes an href, but a protocol
// allowlist only answers "is this safe to put in an href?" — `javascript:` is
// rejected, `https://attacker.example/login` is not. The second question, "is
// this one of OUR Git servers?", it could not ask: the origins live in
// `git_servers`, and the only route that served them was
// `/api/internal/git-servers`, behind `X-Internal-Token`. So the frontend
// shipped the enforcement point (`configureTrustedGitOrigins`) with the data
// source missing, and a drifted or poisoned `source_repo_url` would be
// rendered as a trusted control wearing our chrome.
//
// ── Why a fleet endpoint rather than a per-item field ───────────────────────
//
// The alternative was `ItemResponse.gitServerWebUrl` — each item carrying its
// own server's browser URL. It was rejected for three reasons and one honest
// caveat:
//
//  1. The enforcement point is global. `normalizeRepoBase` consults a
//     module-level allowlist, and every builder (edit / history / archive /
//     availability) funnels through it. A per-item field would have to be
//     threaded into each of those call sites — i.e. the frontend's enforcement
//     point would have to be rebuilt to consume it.
//
//  2. A per-item field would not even cover the surface. `ItemResponse` is
//     produced only by the detail and write endpoints; `GET /api/items`,
//     `/registries/:id/items` and `ListMyItems` serialize models.CapabilityItem
//     directly. Every list card would therefore fail closed while the detail
//     page worked, and fixing that means a batched git_servers join in three
//     more handlers. One fleet fetch covers all of them by construction.
//
//  3. In the common case it is a tautology. `source_repo_url` is written by
//     this server as `gitserver.BrowserBaseURL(...) + "/owner/repo"`, so a
//     per-item `gitServerWebUrl` is by construction the origin prefix of the
//     URL it would be checked against — the same string, repeated once per row,
//     twenty times per page.
//
// The caveat, stated rather than hidden: the per-item field IS strictly tighter
// against one attacker — someone who can rewrite `capability_items.source_repo_url`
// but not `source_git_server_id`. That attacker moves an item to another
// origin, and a fleet allowlist only catches it if the new origin is outside
// the fleet. But an attacker with UPDATE on that table can write the other
// column too, so the extra tightness rests on an assumption about which columns
// they can reach; and the right place to enforce "this item's URL must match
// its own server" is this server at read time, where both values are already in
// hand — not a wire field the client is asked to compare for us. That check is
// a separate, server-side change and does not need this endpoint's shape.
//
// ── Exposure ────────────────────────────────────────────────────────────────
//
// Public (OptionalAuth), for the same reason the gap is worth closing:
// anonymous callers browse public Git-backed items and get repository controls
// rendered for them, so gating the allowlist behind login would leave exactly
// those callers on the protocol-only check they are being protected from.
//
// What that discloses is the set of Git server origins — which every public
// Git-backed item already discloses verbatim in its own `sourceRepoUrl`, on
// the same anonymous path. It is derived from the same expression, so it adds
// no origin that browse does not already emit for a server hosting one public
// item. A server hosting only private items is newly named; that is a hostname
// for an independently authenticated service, not a credential, and it is the
// price of protecting anonymous readers.
//
// What it must never disclose is the rest of the row: `config` holds
// admin_token, webhook_secret and internal_token, and `endpoint` is the
// cluster-internal API address. Neither is projected. The endpoint fallback
// below is not an exception to that — see the comment on the response builder.

package handlers

import (
	"log"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
)

// GitTrustedOriginsResponse is the deployment's Git server origins.
//
// Origins, not base URLs: the consumer compares `new URL(x).origin`, so
// shipping a path would imply a precision the enforcement does not have.
// Deriving the origin here also means the parse happens once, in Go, and
// strips any path/query/userinfo an operator left in the free-text
// `config.web_url`.
//
// Never null and never omitted. An empty array is a meaningful answer — "this
// deployment has no Git servers" — and it is materially different from a
// missing field, which the frontend reads as "no allowlist configured" and
// answers with today's protocol-only behaviour.
type GitTrustedOriginsResponse struct {
	Origins []string `json:"origins"`
}

// ListTrustedGitOrigins godoc
// @Summary      List trusted Git server origins
// @Description  Browser-reachable origins (scheme://host[:port]) of this deployment's enabled Git servers, for clients that must verify a repository URL belongs to this fleet before rendering it as a link. Derived from the same address the server uses to build repository coordinates: config.web_url when set, otherwise the API endpoint. Carries no credentials, no server identifiers and no cluster-internal detail beyond that address. Public read.
// @Tags         git-servers
// @Produce      json
// @Success      200  {object}  GitTrustedOriginsResponse
// @Failure      500  {object}  object{error=string}
// @Router       /git-servers/trusted-origins [get]
func ListTrustedGitOrigins(c *gin.Context) {
	db := database.GetDB()

	var servers []models.GitServer
	// Enabled only. `enabled=false` is the operator's "stop using this server",
	// and gitserver.ResolveByServerID already refuses it — items still bound to
	// it cannot have their content read, so their repository link degrading to
	// "not a link we can open" is consistent with the rest of the UI rather
	// than a new failure. It is also the fail-closed answer to a decommissioned
	// host whose domain someone else later registers.
	if err := db.WithContext(c.Request.Context()).
		Where("enabled = ?", true).
		Order("server_id ASC").
		Find(&servers).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load git server origins"})
		return
	}

	c.JSON(http.StatusOK, GitTrustedOriginsResponse{Origins: trustedGitOrigins(servers)})
}

// trustedGitOrigins projects rows to deduplicated origins, dropping any row it
// cannot make a truthful claim about.
//
// The web_url → endpoint fallback is inherited from gitserver.BrowserBaseURL
// and is not optional. That function is what writes `source_repo_url`, so on a
// single-address deployment (local dev, and any install where the API host IS
// the browser host) every stored coordinate already starts with the endpoint.
// Listing only `web_url` would produce an allowlist NARROWER than the set of
// origins this server itself wrote, and the symptom would be every legitimate
// repository link on that deployment silently going dead. So the endpoint
// origin appears here exactly when it already appears in item responses, and
// never otherwise: this leaks nothing that browse does not.
func trustedGitOrigins(servers []models.GitServer) []string {
	origins := make([]string, 0, len(servers))
	seen := make(map[string]struct{}, len(servers))
	for _, server := range servers {
		base, err := gitserver.ServerBrowserBaseURL(server)
		if err != nil {
			// Unparseable config: we cannot read web_url, so we cannot say
			// which address is browser-facing. Skipping is the fail-closed
			// direction and the operator needs to know, so it is logged with
			// the server id and nothing else — the blob itself holds secrets.
			log.Printf("git trusted origins: skipping server %s: config unreadable", server.ServerID)
			continue
		}
		origin, ok := gitserver.BrowserOrigin(base)
		if !ok {
			// A configured address that is not an http(s) URL with a host names
			// no place a browser may be sent.
			log.Printf("git trusted origins: skipping server %s: no browser-reachable address", server.ServerID)
			continue
		}
		if _, duplicate := seen[origin]; duplicate {
			// Several servers may share one host (different admin tokens, one
			// Gitea). The allowlist is a set.
			continue
		}
		seen[origin] = struct{}{}
		origins = append(origins, origin)
	}
	return origins
}
