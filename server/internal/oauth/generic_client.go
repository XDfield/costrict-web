// Package oauth provides a provider-agnostic OAuth2 / OIDC client used by the
// multi-IdP login flow (Phase E2.6). Unlike the casdoor client which is
// hard-wired to Casdoor's endpoints and schema, this client takes a runtime
// IdP config (fetched from cs-user's idp_sources) and speaks standard OAuth2
// authorization-code flow against any compliant provider (GitHub, Google,
// Azure AD, Feishu, etc.).
//
// Userinfo is normalized to a generic Profile struct. Provider-specific field
// aliases are applied via optional FieldMap (typically sourced from cs-user's
// provider_mapping so the mapping table is the single source of truth).
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrInvalidConfig means the IdP config is missing a required field.
	ErrInvalidConfig = errors.New("oauth: invalid idp config")
	// ErrTokenExchange means the token endpoint returned non-200 / empty token.
	ErrTokenExchange = errors.New("oauth: token exchange failed")
	// ErrUserInfo means the userinfo endpoint returned non-200 or decode error.
	ErrUserInfo = errors.New("oauth: userinfo fetch failed")
)

// Config is the minimal OAuth2 / OIDC config extracted from cs-user's
// idp_sources.Config map. Field names match cs-user's validator
// (idp/validation.go).
type Config struct {
	AuthorizationURL string
	TokenURL         string
	UserInfoURL      string
	ClientID         string
	ClientSecret     string
	Scopes           []string
}

// FieldMap aliases provider-specific userinfo keys to normalized Profile
// fields. Empty entries fall through to the default OIDC key set.
type FieldMap struct {
	Subject    string // default: "sub" | "id"
	Email      string // default: "email"
	Name       string // default: "name" | "displayName"
	Username   string // default: "preferred_username" | "login"
	AvatarURL  string // default: "picture" | "avatar_url"
}

// Profile is the normalized userinfo consumed by user provisioning.
type Profile struct {
	Subject    string `json:"subject"`
	Email      string `json:"email,omitempty"`
	Name       string `json:"name,omitempty"`
	Username   string `json:"username,omitempty"`
	AvatarURL  string `json:"avatar_url,omitempty"`
	Raw        map[string]interface{} `json:"-"` // original userinfo for debugging
}

// TokenResponse mirrors a standard OAuth2 token response.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken     string `json:"id_token,omitempty"`
}

// Client speaks standard OAuth2 against any provider described by Config.
type Client struct {
	httpClient *http.Client
}

// NewClient returns a Client with a sane default timeout.
func NewClient() *Client {
	return &Client{httpClient: &http.Client{Timeout: 15 * time.Second}}
}

// ParseConfig extracts a Config from a cs-user idp_sources Config map.
// Returns ErrInvalidConfig if any required URL or credential is missing.
func ParseConfig(raw map[string]interface{}) (Config, error) {
	str := func(k string) string {
		if v, ok := raw[k]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
		return ""
	}
	cfg := Config{
		AuthorizationURL: str("authorization_url"),
		TokenURL:         str("token_url"),
		UserInfoURL:      str("userinfo_url"),
		ClientID:         str("client_id"),
		ClientSecret:     str("client_secret"),
	}
	// scopes may be string or []string
	if v, ok := raw["scopes"]; ok {
		switch s := v.(type) {
		case string:
			if s != "" {
				cfg.Scopes = strings.Split(s, " ")
			}
		case []interface{}:
			for _, item := range s {
				if str, ok := item.(string); ok {
					cfg.Scopes = append(cfg.Scopes, str)
				}
			}
		case []string:
			cfg.Scopes = append(cfg.Scopes, s...)
		}
	}
	if cfg.AuthorizationURL == "" || cfg.TokenURL == "" || cfg.UserInfoURL == "" {
		return cfg, fmt.Errorf("%w: missing required URL field", ErrInvalidConfig)
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return cfg, fmt.Errorf("%w: missing client_id or client_secret", ErrInvalidConfig)
	}
	return cfg, nil
}

// AuthorizationURL builds the provider's authorize endpoint URL with the
// standard params (response_type=code, scope joined by spaces).
func (c *Client) AuthorizationURL(cfg Config, redirectURI, state string) (string, error) {
	if cfg.AuthorizationURL == "" || cfg.ClientID == "" {
		return "", fmt.Errorf("%w: authorization_url or client_id empty", ErrInvalidConfig)
	}
	params := url.Values{}
	params.Set("client_id", cfg.ClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	if len(cfg.Scopes) > 0 {
		params.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	params.Set("state", state)

	sep := "?"
	if strings.Contains(cfg.AuthorizationURL, "?") {
		sep = "&"
	}
	return cfg.AuthorizationURL + sep + params.Encode(), nil
}

// ExchangeCodeForToken POSTs the authorization code to the token endpoint
// using standard form-encoded body (RFC 6749 §4.1.3).
func (c *Client) ExchangeCodeForToken(ctx context.Context, cfg Config, code, redirectURI string) (*TokenResponse, error) {
	if cfg.TokenURL == "" || cfg.ClientID == "" {
		return nil, fmt.Errorf("%w: token_url or client_id empty", ErrInvalidConfig)
	}
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", cfg.ClientID)
	form.Set("client_secret", cfg.ClientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("oauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: token request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("oauth: read token body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d: %s", ErrTokenExchange, resp.StatusCode, truncate(string(body), 200))
	}

	// Many providers (GitHub historically) return non-JSON error objects when
	// something is wrong; detect by checking content-type / first char.
	var token TokenResponse
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, fmt.Errorf("%w: decode (body=%s): %v", ErrTokenExchange, truncate(string(body), 200), err)
	}
	if token.AccessToken == "" {
		return nil, fmt.Errorf("%w: empty access_token in response: %s", ErrTokenExchange, truncate(string(body), 200))
	}
	return &token, nil
}

// FetchUserInfo GETs the userinfo endpoint with a Bearer token and normalizes
// the response using the (optional) FieldMap.
func (c *Client) FetchUserInfo(ctx context.Context, cfg Config, accessToken string, fm FieldMap) (*Profile, error) {
	if cfg.UserInfoURL == "" {
		return nil, fmt.Errorf("%w: userinfo_url empty", ErrInvalidConfig)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.UserInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("oauth: build userinfo request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: userinfo request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("oauth: read userinfo body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d: %s", ErrUserInfo, resp.StatusCode, truncate(string(body), 200))
	}

	var raw map[string]interface{}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrUserInfo, err)
	}

	return normalizeProfile(raw, fm), nil
}

// normalizeProfile applies the FieldMap aliases to a raw userinfo map and
// produces a Profile. Falls back through common OIDC + GitHub key sets.
func normalizeProfile(raw map[string]interface{}, fm FieldMap) *Profile {
	pickStr := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := raw[k]; ok {
				switch s := v.(type) {
				case string:
					if s != "" {
						return s
					}
				case float64: // JSON numbers
					return fmt.Sprintf("%v", s)
				case json.Number:
					return s.String()
				}
			}
		}
		return ""
	}

	subjectKeys := []string{fm.Subject, "sub", "id"}
	emailKeys := []string{fm.Email, "email"}
	nameKeys := []string{fm.Name, "name", "displayName"}
	usernameKeys := []string{fm.Username, "preferred_username", "login", "username"}
	avatarKeys := []string{fm.AvatarURL, "picture", "avatar_url", "avatar"}

	// Filter out empty fm entries so they don't shadow defaults
	subjectKeys = nonEmpty(subjectKeys)
	emailKeys = nonEmpty(emailKeys)
	nameKeys = nonEmpty(nameKeys)
	usernameKeys = nonEmpty(usernameKeys)
	avatarKeys = nonEmpty(avatarKeys)

	return &Profile{
		Subject:   pickStr(subjectKeys...),
		Email:     pickStr(emailKeys...),
		Name:      pickStr(nameKeys...),
		Username:  pickStr(usernameKeys...),
		AvatarURL: pickStr(avatarKeys...),
		Raw:       raw,
	}
}

func nonEmpty(ks []string) []string {
	out := ks[:0]
	for _, k := range ks {
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
