// handlers/auth_multi_idp.go — Phase E2.6 multi-IdP OAuth orchestration.
//
// This file mounts alongside the legacy Casdoor-only Login / AuthCallback in
// handlers.go. When the request carries `?idp=<provider>` (Login) or a state
// token issued by encodeMultiIdPState (Callback), the request is routed to
// the provider-aware flow implemented here; otherwise the legacy Casdoor
// flow runs unchanged, preserving backward compatibility for existing
// frontends that never set the `idp` query param.
//
// State shape (HMAC-signed, base64url payload + "." + hex sig):
//
//	midp.<payload>.<sig>
//
// The `midp.` prefix lets AuthCallback cheaply distinguish multi-IdP state
// from the legacy base64 oauthState and the "."-carrying bindState/mergeState
// (which never start with `midp.`).
//
// Secrets handling: ListIdPs (the public endpoint) strips client_secret,
// bind_password and similar sensitive keys before returning. Login/Callback
// pull raw configs via the internal-token-gated RPC client; those never
// reach a response body.

package handlers

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/idp"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/oauth"
	"github.com/costrict/costrict-web/server/internal/tenant"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// multiIdPStateTTL bounds how long an OAuth `state` token is considered
// valid. 10 minutes matches typical IdP round-trips; overridable via
// AUTH_STATE_TTL_SECONDS for operators with slow users / long IdP detours.
var multiIdPStateTTL = 10 * time.Minute

// multiIdPDefaultProvider is the provider name used when no `?idp=` is
// provided on /api/auth/login AND the multi-IdP handler is wired. Empty
// string means "fall back to legacy Casdoor flow". Set via InitMultiIdP.
var multiIdPDefaultProvider string

// MultiIdPHandler is the provider-aware OAuth orchestrator. Wire it once
// in main.go via InitMultiIdP; the package-level multiIdP singleton is
// then consulted by the Login/Callback wrappers. A nil RPCClient or nil
// OAuthClient is treated as "multi-IdP disabled" — the wrappers fall
// through to the legacy Casdoor flow.
type MultiIdPHandler struct {
	RPC             *idp.RPCClient
	OAuth           *oauth.Client
	UserWriter      userpkg.UserWriter
	StateSecret     string
	DefaultProvider string
	JWTSignMode     string
}

var multiIdP *MultiIdPHandler

// InitMultiIdP wires the singleton handler. main.go calls this once after
// loading config; passing a handler with nil RPC or nil OAuth disables
// the multi-IdP path (login/callback fall through to Casdoor).
func InitMultiIdP(h *MultiIdPHandler) {
	multiIdP = h
	if h != nil {
		multiIdPDefaultProvider = strings.TrimSpace(h.DefaultProvider)
		if h.OAuth == nil {
			h.OAuth = oauth.NewClient()
		}
	}
}

// SetMultiIdPStateTTL overrides the default state token TTL. main.go calls
// this with AUTH_STATE_TTL_SECONDS when set; default remains 10 minutes.
func SetMultiIdPStateTTL(d time.Duration) {
	if d > 0 {
		multiIdPStateTTL = d
	}
}

// multiIdPEnabled reports whether the multi-IdP path is available at all
// (RPC + OAuth client both wired). When false, callers fall back to the
// legacy Casdoor flow.
func multiIdPEnabled() bool {
	return multiIdP != nil &&
		multiIdP.RPC != nil && multiIdP.RPC.Configured() &&
		multiIdP.OAuth != nil
}

// multiIdPStatePayload is the JSON shape carried inside the signed state
// token. Compact keys keep the resulting URL short — IdPs vary in their
// state length tolerance.
type multiIdPStatePayload struct {
	TenantID    string `json:"t"`           // resolved tenant slug/id at login time
	Provider    string `json:"p"`           // provider name
	Nonce       string `json:"n"`           // CSRF nonce echoed back by IdP
	RedirectTo  string `json:"r,omitempty"` // post-login frontend redirect
	CallbackURL string `json:"c,omitempty"` // callback URL on our origin
	IssuedAt    int64  `json:"i"`           // unix seconds — bounded by multiIdPStateTTL
}

// multiIdPStatePrefix tags signed state tokens so the callback can cheaply
// distinguish them from legacy / bind / merge states.
const multiIdPStatePrefix = "midp."

// encodeMultiIdPState serializes + HMAC-signs the state. Returns the full
// `midp.<payload>.<sig>` string sent to the IdP as the `state` query param.
func (h *MultiIdPHandler) encodeMultiIdPState(p multiIdPStatePayload) (string, error) {
	if p.IssuedAt == 0 {
		p.IssuedAt = time.Now().Unix()
	}
	if p.Nonce == "" {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", fmt.Errorf("generate nonce: %w", err)
		}
		p.Nonce = hex.EncodeToString(b[:])
	}
	body, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("marshal state: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	sig := h.signState(payload)
	return multiIdPStatePrefix + payload + "." + sig, nil
}

// decodeMultiIdPState verifies the signature + TTL and returns the payload.
// Returns ok=false on any failure (bad prefix, bad sig, expired). Callers
// treat a failed decode as a CSRF rejection: 400 with no further info.
func (h *MultiIdPHandler) decodeMultiIdPState(raw string) (multiIdPStatePayload, bool) {
	if !strings.HasPrefix(raw, multiIdPStatePrefix) {
		return multiIdPStatePayload{}, false
	}
	rest := strings.TrimPrefix(raw, multiIdPStatePrefix)
	parts := strings.SplitN(rest, ".", 2)
	if len(parts) != 2 {
		return multiIdPStatePayload{}, false
	}
	payload, sig := parts[0], parts[1]
	if !h.verifyState(payload, sig) {
		return multiIdPStatePayload{}, false
	}
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return multiIdPStatePayload{}, false
	}
	var p multiIdPStatePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return multiIdPStatePayload{}, false
	}
	if p.IssuedAt == 0 {
		return multiIdPStatePayload{}, false
	}
	if time.Since(time.Unix(p.IssuedAt, 0)) > multiIdPStateTTL {
		return multiIdPStatePayload{}, false
	}
	return p, true
}

func (h *MultiIdPHandler) signState(payload string) string {
	key := strings.TrimSpace(h.StateSecret)
	if key == "" {
		// Reuse the package-level bindStateSecret (cfg.InternalSecret) so
		// deployments without AUTH_STATE_SECRET keep working. The fallback
		// is only used when InitMultiIdP was called with an empty secret.
		key = strings.TrimSpace(bindStateSecret)
	}
	if key == "" {
		key = "costrict-multi-idp-state-default"
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func (h *MultiIdPHandler) verifyState(payload, sig string) bool {
	expected := h.signState(payload)
	if len(expected) == 0 || len(expected) != len(sig) {
		return false
	}
	return hmac.Equal([]byte(expected), []byte(sig))
}

// LoginMultiIdP wraps the legacy Login. When `?idp=` is set (or a default
// provider is configured and Casdoor is no longer the primary), it runs
// the provider-aware flow; otherwise it delegates to Login (Casdoor).
//
// Mount this at GET /api/auth/login in place of handlers.Login.
func LoginMultiIdP(c *gin.Context) {
	provider := strings.TrimSpace(c.Query("idp"))

	if provider == "" {
		provider = multiIdPDefaultProvider
	}
	if provider == "" || !multiIdPEnabled() {
		Login(c) // legacy Casdoor flow
		return
	}

	runMultiIdPLogin(c, provider)
}

// CallbackMultiIdP wraps the legacy AuthCallback. When the inbound state
// is a multi-IdP token, it runs the provider-aware callback; otherwise it
// delegates to AuthCallback (Casdoor + bind + merge flows).
//
// Mount this at GET /api/auth/callback and GET /api/auth/bind/callback in
// place of handlers.AuthCallback.
func CallbackMultiIdP(c *gin.Context) {
	rawState := c.Query("state")
	if strings.HasPrefix(rawState, multiIdPStatePrefix) {
		runMultiIdPCallback(c)
		return
	}
	AuthCallback(c) // legacy (Casdoor / bind / merge)
}

// runMultiIdPLogin resolves the tenant, fetches the IdP config, builds the
// authorize URL and 302s the user there.
func runMultiIdPLogin(c *gin.Context, provider string) {
	h := multiIdP
	if h == nil || h.RPC == nil || h.OAuth == nil {
		Login(c)
		return
	}

	tenantID := tenantSlugOrEmpty(c)

	redirectTo := c.DefaultQuery("redirect_to", "/")
	callbackURL := c.Query("callback_url")
	if callbackURL != "" && !isAllowedOrigin(callbackURL) {
		callbackURL = ""
	}
	if callbackURL == "" {
		callbackURL = defaultCallbackURL(c)
	}

	view, err := h.RPC.GetIdP(c.Request.Context(), tenantID, provider)
	if err != nil {
		logger.Warn("[multi-idp-login] get idp %q tenant=%q failed: %v", provider, tenantID, err)
		redirectLoginError(c, "idp_unavailable")
		return
	}
	if !view.Enabled {
		logger.Warn("[multi-idp-login] idp %q tenant=%q disabled", provider, tenantID)
		redirectLoginError(c, "idp_disabled")
		return
	}

	cfg, err := oauth.ParseConfig(view.Config)
	if err != nil {
		logger.Warn("[multi-idp-login] parse config %q tenant=%q failed: %v", provider, tenantID, err)
		redirectLoginError(c, "idp_misconfigured")
		return
	}

	state, err := h.encodeMultiIdPState(multiIdPStatePayload{
		TenantID:    tenantID,
		Provider:    provider,
		RedirectTo:  redirectTo,
		CallbackURL: callbackURL,
	})
	if err != nil {
		logger.Warn("[multi-idp-login] encode state failed: %v", err)
		redirectLoginError(c, "internal_error")
		return
	}

	authURL, err := h.OAuth.AuthorizationURL(cfg, callbackURL, state)
	if err != nil {
		logger.Warn("[multi-idp-login] build auth url failed: %v", err)
		redirectLoginError(c, "idp_misconfigured")
		return
	}

	logger.Info("[multi-idp-login] redirect tenant=%q provider=%q", tenantID, provider)
	c.Redirect(http.StatusFound, authURL)
}

// runMultiIdPCallback exchanges the code, fetches userinfo, provisions the
// user via UserWriter and sets the auth cookie.
func runMultiIdPCallback(c *gin.Context) {
	h := multiIdP
	if h == nil || h.RPC == nil || h.OAuth == nil {
		AuthCallback(c)
		return
	}

	rawState := c.Query("state")
	payload, ok := h.decodeMultiIdPState(rawState)
	if !ok {
		logger.Warn("[multi-idp-callback] state invalid or expired")
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_or_expired_state"})
		return
	}

	code := c.Query("code")
	if code == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "code is required"})
		return
	}

	// Re-fetch the IdP config — state only carries routing, not config.
	view, err := h.RPC.GetIdP(c.Request.Context(), payload.TenantID, payload.Provider)
	if err != nil {
		logger.Warn("[multi-idp-callback] get idp tenant=%q provider=%q failed: %v",
			payload.TenantID, payload.Provider, err)
		redirectLoginError(c, "idp_unavailable")
		return
	}
	if !view.Enabled {
		logger.Warn("[multi-idp-callback] idp disabled mid-flow tenant=%q provider=%q",
			payload.TenantID, payload.Provider)
		redirectLoginError(c, "idp_disabled")
		return
	}

	cfg, err := oauth.ParseConfig(view.Config)
	if err != nil {
		logger.Warn("[multi-idp-callback] parse config failed: %v", err)
		redirectLoginError(c, "idp_misconfigured")
		return
	}

	ctx := c.Request.Context()

	tokenResp, err := h.OAuth.ExchangeCodeForToken(ctx, cfg, code, payload.CallbackURL)
	if err != nil {
		logger.Warn("[multi-idp-callback] token exchange failed: %v", err)
		redirectLoginError(c, "token_exchange_failed")
		return
	}

	fm := extractFieldMap(view.Config)
	profile, err := h.OAuth.FetchUserInfo(ctx, cfg, tokenResp.AccessToken, fm)
	if err != nil {
		logger.Warn("[multi-idp-callback] userinfo fetch failed: %v", err)
		redirectLoginError(c, "userinfo_failed")
		return
	}

	// Normalize OAuth Profile → JWTClaims. Subject and Email are the two
	// load-bearing fields; the rest is best-effort enrichment.
	// profile.Raw (the full IdP userinfo map) is forwarded as ExternalClaims
	// so cs-user's ApplyEnterpriseMapping can run field_map extraction on it.
	claims := &userpkg.JWTClaims{
		Sub:               profile.Subject,
		Email:             profile.Email,
		Name:              profile.Name,
		PreferredUsername: profile.Username,
		Picture:           profile.AvatarURL,
		Provider:          payload.Provider,
		ProviderUserID:    profile.Subject,
		ExternalClaims:    profile.Raw,
	}

	// Carry the tenant slug through the upcoming cs-user writes so they
	// forward the right X-Tenant-Id.
	if payload.TenantID != "" {
		ctx = tenant.WithSlug(ctx, payload.TenantID)
		c.Request = c.Request.WithContext(ctx)
	}

	writer := h.UserWriter
	if writer == nil && UserModule != nil {
		writer = UserModule.Writer
	}
	if writer == nil {
		logger.Warn("[multi-idp-callback] no UserWriter wired")
		redirectLoginError(c, "internal_error")
		return
	}

	created, err := writer.GetOrCreateUser(ctx, claims)
	if err != nil || created == nil {
		logger.Warn("[multi-idp-callback] GetOrCreateUser failed: %v", err)
		redirectLoginError(c, "provisioning_failed")
		return
	}

	// Bind the identity idempotently. Ignore "already bound" outcomes —
	// cs-user treats repeat bindings as success.
	_ = writer.BindIdentityToUser(ctx, created.SubjectID, claims)

	// Enterprise mapping is auto-triggered inside cs-user's GetOrCreateUser
	// (using claims.ExternalClaims harvested from profile.Raw above). No
	// explicit server-side call needed — it would be a downgrade (no claims).

	cookieToken := tokenResp.AccessToken
	if h.JWTSignMode != "" && h.JWTSignMode != config.JWTSignModeOff {
		if newTok, _, err := writer.ReissueToken(ctx, created.SubjectID, claims, nil); err != nil {
			logger.Warn("[multi-idp-callback] ReissueToken failed (falling back to provider token): %v", err)
		} else {
			cookieToken = newTok
		}
	}

	c.SetCookie("zgsmAdminToken", cookieToken, int(7*24*time.Hour/time.Second), "/", "", cookieSecure, false)

	// Sticky tenant cookie so subsequent requests carry the tenant slug.
	if payload.TenantID != "" {
		c.SetCookie(middleware.TenantSlugCookie, payload.TenantID,
			int(365*24*time.Hour/time.Second), "/", "", cookieSecure, true)
	}

	c.Redirect(http.StatusFound, resolvePostLoginRedirect(payload.RedirectTo))
}

// ListIdPs godoc
// @Summary      List enabled IdPs for the current tenant
// @Description  Returns the tenant's enabled identity providers (secrets redacted). Tenant resolution follows the standard X-Tenant-Id / cs_tenant_slug / subdomain chain.
// @Tags         auth
// @Produce      json
// @Success      200  {array}  object{provider=string,display_name=string,icon_hint=string,priority=int}
// @Failure      503  {object}  object{error=string}
// @Router       /auth/idps [get]
func ListIdPs(c *gin.Context) {
	if !multiIdPEnabled() {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "multi-IdP flow not configured",
		})
		return
	}
	tenantID := tenantSlugOrEmpty(c)
	if tenantID == "" {
		c.JSON(http.StatusOK, []gin.H{})
		return
	}
	views, err := multiIdP.RPC.ListEnabledIdPs(c.Request.Context(), tenantID)
	if err != nil {
		logger.Warn("[multi-idp-list] tenant=%q failed: %v", tenantID, err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "idp service unavailable"})
		return
	}
	out := make([]gin.H, 0, len(views))
	for _, v := range views {
		if !v.Enabled {
			continue
		}
		out = append(out, gin.H{
			"provider":     v.Provider,
			"display_name": displayNameForProvider(v.Provider, v.Config),
			"icon_hint":    v.Provider,
			"priority":     v.Priority,
		})
	}
	c.JSON(http.StatusOK, out)
}

// extractFieldMap pulls an optional provider_mapping field_map from the
// IdP config. cs-user's provider_mapping table is the source of truth;
// when an admin has embedded a field_map directly into the IdP config
// (e.g. for one-off providers), we honor it here.
func extractFieldMap(cfg map[string]interface{}) oauth.FieldMap {
	fm := oauth.FieldMap{}
	if raw, ok := cfg["field_map"]; ok {
		if m, ok := raw.(map[string]interface{}); ok {
			if s, ok := m["subject"].(string); ok {
				fm.Subject = s
			}
			if s, ok := m["email"].(string); ok {
				fm.Email = s
			}
			if s, ok := m["name"].(string); ok {
				fm.Name = s
			}
			if s, ok := m["username"].(string); ok {
				fm.Username = s
			}
			if s, ok := m["avatar_url"].(string); ok {
				fm.AvatarURL = s
			}
		}
	}
	return fm
}

// displayNameForProvider returns a friendly name for the provider. Falls
// back to the provider key when no display_name is set in config.
func displayNameForProvider(provider string, cfg map[string]interface{}) string {
	if v, ok := cfg["display_name"]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	switch strings.ToLower(provider) {
	case "github":
		return "GitHub"
	case "google":
		return "Google"
	case "azure", "azure_ad", "azuread":
		return "Microsoft"
	case "feishu", "lark":
		return "Feishu"
	case "gitlab":
		return "GitLab"
	case "casdoor":
		return "Casdoor"
	default:
		return provider
	}
}

// resolvePostLoginRedirect mirrors AuthCallback's redirect logic for the
// post-login jump: full allowed URLs go through, plain paths get prefixed
// with defaultFrontendURL, disallowed absolute URLs fall through to the
// frontend root.
func resolvePostLoginRedirect(redirectTo string) string {
	if redirectTo == "" {
		return defaultFrontendURL + "/"
	}
	if isAllowedOrigin(redirectTo) {
		return redirectTo
	}
	if !strings.HasPrefix(redirectTo, "http://") && !strings.HasPrefix(redirectTo, "https://") {
		return defaultFrontendURL + redirectTo
	}
	return defaultFrontendURL + "/"
}

// redirectLoginError sends the browser to the frontend login page with an
// error code. The frontend decides how to surface it.
func redirectLoginError(c *gin.Context, code string) {
	target := defaultFrontendURL + "/login?error=" + code
	c.Redirect(http.StatusFound, target)
}

// tenantSlugOrEmpty pulls the resolved tenant slug from the request
// context (set by ResolveTenantSlug middleware). Empty means "no signal".
func tenantSlugOrEmpty(c *gin.Context) string {
	if v, ok := c.Get("tenant_slug"); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return tenant.SlugFromContext(c.Request.Context())
}

// defaultCallbackURL derives /api/auth/callback on the current request's
// origin. Used when the caller didn't pass callback_url.
func defaultCallbackURL(c *gin.Context) string {
	scheme := "https"
	if c.Request.TLS == nil {
		if x := c.GetHeader("X-Forwarded-Proto"); x != "" {
			scheme = x
		} else {
			scheme = "http"
		}
	}
	host := c.Request.Host
	if host == "" {
		host = c.Request.URL.Host
	}
	return scheme + "://" + host + "/api/auth/callback"
}
