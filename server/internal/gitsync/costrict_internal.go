// CoStrict fork private-surface client (Gitea fork integration FI-4 / FI-5).
//
// The zgsm-ai/gitea fork mounts three endpoints under Gitea's own internal API
// (routers/private/internal.go), which costrict-web uses to push push-quota
// rules and to force a JWKS re-fetch after a signing-key rotation:
//
//	GET  {endpoint}/api/internal/costrict/healthz
//	POST {endpoint}/api/internal/costrict/quota-invalidate   {"rules":[...]}
//	POST {endpoint}/api/internal/costrict/jwks-invalidate    (no body)
//
// This is a SEPARATE client from gitsync.Client on purpose. Everything about
// the credential differs:
//
//   - the token is Gitea's [security] INTERNAL_TOKEN, not an admin PAT;
//   - it travels in X-Gitea-Internal-Auth, not Authorization (the fork's
//     authInternal reads only that header — sending Authorization gets a 403
//     that looks exactly like "endpoint does not exist");
//   - the prefix is "Bearer " matched with strings.CutPrefix, i.e. case
//     sensitive and exactly one space, unlike the case-insensitive Bearer
//     handling elsewhere in Gitea.
//
// Reusing Client.doJSON would have silently sent the wrong header.
//
// Two fork behaviours are load-bearing and are defended against here rather
// than documented and hoped for:
//
//  1. QuotaCacheInvalidate binds its body through a helper that DISCARDS the
//     binding error. A request whose Content-Type is not JSON therefore
//     produces Rules == nil, which Refresh() applies as "replace the whole rule
//     set with nothing" — every rule is wiped and the response is still
//     200 {"user_msg":"ok"}. Content-Type is set unconditionally below and must
//     stay that way.
//  2. Refresh() REPLACES; it does not merge. Callers must always send the
//     complete snapshot for the server, never a delta.
//
// Neither endpoint is routed through internal/safetch: safetch rejects
// loopback and private addresses, and a self-hosted Gitea is normally exactly
// that. These URLs come from operator-managed git_servers rows, not user input.
package gitsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors for the fork-private surface.
var (
	// ErrCostrictQuotaDisabled — the fork answered 200 but reported that its
	// quota feature is off ([costrict] ENABLED=false or QUOTA_ENABLED=false),
	// so it discarded the rules we just sent. HTTP-wise a success; in every way
	// that matters a failure, and never to be recorded as "rules delivered".
	ErrCostrictQuotaDisabled = errors.New("gitsync: costrict quota is disabled on the Git server (rules were discarded)")

	// ErrCostrictDisabled — the whole fork surface is gated off
	// ([costrict] ENABLED=false), so jwks-invalidate did nothing.
	ErrCostrictDisabled = errors.New("gitsync: costrict fork surface is disabled on the Git server (request was a no-op)")

	// ErrCostrictUnexpectedResponse — 200 with a user_msg we do not recognise.
	// Deliberately an error: an unrecognised acknowledgement is not evidence
	// that the fork accepted anything.
	ErrCostrictUnexpectedResponse = errors.New("gitsync: unexpected response from costrict private endpoint")
)

// Fork acknowledgement strings, quoted verbatim from routers/private/costrict.go.
const (
	costrictUserMsgOK             = "ok"
	costrictUserMsgQuotaDisabled  = "costrict quota disabled, no-op"
	costrictUserMsgForkDisabled   = "costrict disabled, no-op"
	costrictInternalAuthHeader    = "X-Gitea-Internal-Auth"
	costrictQuotaInvalidatePath   = "/api/internal/costrict/quota-invalidate"
	costrictJWKSInvalidatePath    = "/api/internal/costrict/jwks-invalidate"
	costrictHealthzPath           = "/api/internal/costrict/healthz"
	costrictInternalDefaultTimout = 10 * time.Second
)

// QuotaRule is one row of the fork's push-quota matrix.
//
// Field-for-field identical to modules/costrict/quota.Rule in the fork,
// including the JSON names — this struct IS the wire contract, so it must not
// grow fields the fork does not read.
//
//	Repo == ""          → the owner-level default
//	MaxFileSizeMB == 0  → no per-file limit
//	RepoQuotaMB == 0    → no repository total limit
type QuotaRule struct {
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	MaxFileSizeMB int64  `json:"max_file_size_mb"`
	RepoQuotaMB   int64  `json:"repo_quota_mb"`
}

// quotaRulePayload is the request body of quota-invalidate.
type quotaRulePayload struct {
	Rules []QuotaRule `json:"rules"`
}

// costrictResponse is the fork's private.Response shape.
type costrictResponse struct {
	Err     string `json:"err,omitempty"`
	UserMsg string `json:"user_msg,omitempty"`
}

// CostrictHealth is the healthz payload. Note that Version is a hardcoded
// constant on the fork side ("poc-1"), so it identifies the fork surface but
// NOT which build is deployed — use /api/v1/version for that.
type CostrictHealth struct {
	Enabled      bool   `json:"enabled"`
	QuotaEnabled bool   `json:"quota_enabled"`
	JWKSURL      string `json:"jwks_url"`
	Version      string `json:"version"`
}

// CostrictInternalClient talks to one Git server's fork-private endpoints.
type CostrictInternalClient struct {
	baseURL       string
	internalToken string
	httpClient    *http.Client
}

// NewCostrictInternalClient binds a client to a Git server root URL and that
// server's Gitea INTERNAL_TOKEN.
//
// Returns nil when either is empty, matching NewClient's convention: callers
// treat nil as "not configured for this server" and skip it rather than
// sending unauthenticated requests that would 403.
func NewCostrictInternalClient(baseURL, internalToken string) *CostrictInternalClient {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	internalToken = strings.TrimSpace(internalToken)
	if baseURL == "" || internalToken == "" {
		return nil
	}
	return &CostrictInternalClient{
		baseURL:       baseURL,
		internalToken: internalToken,
		httpClient:    &http.Client{Timeout: costrictInternalDefaultTimout},
	}
}

// newCostrictInternalClientWithHTTPC is the test-only constructor.
func newCostrictInternalClientWithHTTPC(baseURL, internalToken string, hc *http.Client) *CostrictInternalClient {
	client := NewCostrictInternalClient(baseURL, internalToken)
	if client == nil {
		return nil
	}
	if hc != nil {
		client.httpClient = hc
	}
	return client
}

// InvalidateQuotaCache replaces the Git server's entire in-memory quota rule
// set with the supplied snapshot.
//
// rules must be the COMPLETE set for this server: the fork's Refresh() rebuilds
// its map from what arrives, so anything omitted is deleted. Passing an empty
// slice is a legitimate "this server has no per-owner overrides" instruction —
// the fork then falls back to its app.ini defaults for every repository.
func (c *CostrictInternalClient) InvalidateQuotaCache(ctx context.Context, rules []QuotaRule) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	// Marshal an empty array rather than null for an empty set. Both decode to
	// a nil slice on the fork side, but "rules":[] states the intent on the
	// wire and keeps the request distinguishable from a malformed body in a
	// packet capture.
	if rules == nil {
		rules = []QuotaRule{}
	}
	resp, err := c.post(ctx, costrictQuotaInvalidatePath, quotaRulePayload{Rules: rules})
	if err != nil {
		return err
	}
	switch resp.UserMsg {
	case costrictUserMsgOK:
		return nil
	case costrictUserMsgQuotaDisabled:
		return ErrCostrictQuotaDisabled
	default:
		return fmt.Errorf("%w: quota-invalidate user_msg=%q err=%q",
			ErrCostrictUnexpectedResponse, resp.UserMsg, resp.Err)
	}
}

// InvalidateJWKSCache forces the Git server to re-fetch the JWKS on its next
// JWT verification, closing the window (up to JWT_JWKS_REFRESH_INTERVAL, 5
// minutes by default) in which tokens signed by a freshly rotated key are
// rejected because the fork has never seen their kid.
//
// It must only be called once the new JWKS is actually servable: the fork's
// Invalidate() drops its cached keys outright, which also drops its
// stale-key fallback, so invalidating against an unreachable JWKS turns a
// degraded state into a total authentication outage.
func (c *CostrictInternalClient) InvalidateJWKSCache(ctx context.Context) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	// No body by design — the fork route registers no binder for this endpoint
	// and ignores anything sent.
	resp, err := c.post(ctx, costrictJWKSInvalidatePath, nil)
	if err != nil {
		return err
	}
	switch resp.UserMsg {
	case costrictUserMsgOK:
		return nil
	case costrictUserMsgForkDisabled:
		return ErrCostrictDisabled
	default:
		return fmt.Errorf("%w: jwks-invalidate user_msg=%q err=%q",
			ErrCostrictUnexpectedResponse, resp.UserMsg, resp.Err)
	}
}

// Healthz reports the fork surface state. Useful for operator diagnostics; the
// reconciler does not depend on it, because a healthz probe answering
// "enabled" is not evidence that a subsequent write succeeded.
func (c *CostrictInternalClient) Healthz(ctx context.Context) (*CostrictHealth, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+costrictHealthzPath, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrGiteaUnreachable, err)
	}
	c.setAuth(req)
	raw, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var health CostrictHealth
	if err := json.Unmarshal(raw, &health); err != nil {
		return nil, fmt.Errorf("%w: decode healthz: %v", ErrGiteaUnreachable, err)
	}
	return &health, nil
}

// post sends a JSON POST and decodes the fork's private.Response.
//
// body == nil still sends Content-Type: application/json with an empty JSON
// object. That is not decoration: see the package comment — a POST to
// quota-invalidate without a JSON content type silently wipes every rule and
// reports success, so the header is never conditional on there being a body.
func (c *CostrictInternalClient) post(ctx context.Context, path string, body any) (*costrictResponse, error) {
	payload := []byte("{}")
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("gitsync: marshal costrict payload: %w", err)
		}
		payload = encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: build request: %v", ErrGiteaUnreachable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)

	raw, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var decoded costrictResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &decoded, nil
}

// setAuth applies the fork's internal-auth header. The scheme prefix is
// "Bearer " exactly — the fork compares with strings.CutPrefix, so neither the
// capitalisation nor the single space is negotiable.
func (c *CostrictInternalClient) setAuth(req *http.Request) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set(costrictInternalAuthHeader, "Bearer "+c.internalToken)
}

// do executes the request and returns the body of a 200 response.
func (c *CostrictInternalClient) do(req *http.Request) ([]byte, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrGiteaTimeout
		}
		return nil, fmt.Errorf("%w: %v", ErrGiteaUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if req.Context().Err() != nil {
		return nil, ErrGiteaTimeout
	}

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode != http.StatusOK {
		snippet := raw
		if len(snippet) > 512 {
			snippet = snippet[:512]
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			// The fork returns 403 both when INTERNAL_TOKEN is unset on its
			// side and when ours does not match, so the message names both.
			return nil, fmt.Errorf("%w: status=%d body=%s (check git_servers.config.internal_token against Gitea [security] INTERNAL_TOKEN)",
				ErrGiteaUnauthorized, resp.StatusCode, snippet)
		case http.StatusNotFound:
			// Upstream Gitea answers 404 here with a valid internal-auth
			// header; the fork answers 200. That difference is the cleanest
			// runtime signal that the deployed binary is not the fork.
			return nil, fmt.Errorf("%w: status=404 body=%s (costrict private endpoints absent — is this the CoStrict Gitea fork?)",
				ErrGiteaTeamNotFound, snippet)
		default:
			return nil, fmt.Errorf("%w: status=%d body=%s", ErrGiteaUnreachable, resp.StatusCode, snippet)
		}
	}
	if readErr != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrGiteaUnreachable, readErr)
	}
	return raw, nil
}
