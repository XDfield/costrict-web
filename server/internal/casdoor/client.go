package casdoor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/costrict/costrict-web/server/internal/config"
)

type CasdoorClient struct {
	endpoint         string // public URL for browser-facing URLs
	internalEndpoint string // internal URL for server-to-server calls
	clientID         string
	secret           string
	callbackURL      string
	organization     string // Casdoor organization name
}

type CasdoorUser struct {
	Sub              string `json:"sub"`
	Id               string `json:"id"`
	Name             string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Email            string `json:"email"`
	Picture          string `json:"picture"`
	Owner            string `json:"owner"`
}

// UnmarshalJSON handles both OIDC (snake_case) and Casdoor admin API (camelCase) formats.
func (u *CasdoorUser) UnmarshalJSON(data []byte) error {
	// Use a raw map to inspect all keys
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	str := func(keys ...string) string {
		for _, key := range keys {
			if v, ok := raw[key]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}

	// Match fields across OIDC snake_case and Casdoor camelCase formats
	u.Sub = str("sub")
	u.Id = str("id")
	u.Name = str("name")
	u.PreferredUsername = str("preferred_username", "displayName")
	u.Email = str("email")
	u.Picture = str("picture", "avatar")
	u.Owner = str("owner")

	// Casdoor admin API doesn't have "sub"; synthesize it as "owner/name"
	// to match the OIDC sub format that the frontend stores as createdBy.
	if u.Sub == "" && u.Owner != "" && u.Name != "" {
		u.Sub = u.Owner + "/" + u.Name
	}

	return nil
}

type CasdoorTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

type CasdoorUserInfoResponse struct {
	User *CasdoorUser
}

func NewClient(cfg *config.CasdoorConfig) *CasdoorClient {
	internal := cfg.InternalEndpoint
	if internal == "" {
		internal = cfg.Endpoint
	}
	return &CasdoorClient{
		endpoint:         cfg.Endpoint,
		internalEndpoint: internal,
		clientID:         cfg.ClientID,
		secret:           cfg.Secret,
		callbackURL:      cfg.CallbackURL,
		organization:     cfg.Organization,
	}
}

// GetLoginURL returns the Casdoor OAuth authorization URL.
// If callbackURL is non-empty it overrides the configured default, allowing
// the frontend to specify its own origin so that Casdoor redirects back
// through the frontend host (important for correct cookie domain).
func (c *CasdoorClient) GetLoginURL(state, callbackURL string) string {
	if callbackURL == "" {
		callbackURL = c.callbackURL
	}
	params := url.Values{}
	params.Set("client_id", c.clientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", callbackURL)
	params.Set("scope", "openid profile email")
	params.Set("state", state)

	return fmt.Sprintf("%s/login/oauth/authorize?%s", c.endpoint, params.Encode())
}

// GetLoginURLWithCallback returns a Casdoor login URL using a custom callback URL
func (c *CasdoorClient) GetLoginURLWithCallback(state, callbackURL string) string {
	params := url.Values{}
	params.Set("client_id", c.clientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", callbackURL)
	params.Set("scope", "openid profile email")
	params.Set("state", state)

	return fmt.Sprintf("%s/login/oauth/authorize?%s", c.endpoint, params.Encode())
}

// ExchangeCodeForToken exchanges an authorization code for an access token.
// If callbackURL is non-empty it overrides the configured default; the value
// must match the redirect_uri used in the authorization request.
func (c *CasdoorClient) ExchangeCodeForToken(code, callbackURL string) (*CasdoorTokenResponse, error) {
	if callbackURL == "" {
		callbackURL = c.callbackURL
	}
	tokenURL := fmt.Sprintf("%s/api/login/oauth/access_token", c.internalEndpoint)

	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("client_id", c.clientID)
	data.Set("client_secret", c.secret)
	data.Set("code", code)
	data.Set("redirect_uri", callbackURL)

	resp, err := http.PostForm(tokenURL, data)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code for token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var tokenResp CasdoorTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal token response: %w", err)
	}

	return &tokenResp, nil
}

// GetUserInfo retrieves user information from Casdoor /api/userinfo (OIDC standard)
func (c *CasdoorClient) GetUserInfo(accessToken string) (*CasdoorUserInfoResponse, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/userinfo", c.internalEndpoint), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo returned %d", resp.StatusCode)
	}

	var user CasdoorUser
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user info: %w", err)
	}

	return &CasdoorUserInfoResponse{User: &user}, nil
}

// CallCasdoorAPI makes a generic API call to Casdoor
func (c *CasdoorClient) CallCasdoorAPI(method, endpoint string, accessToken string, body interface{}) ([]byte, error) {
	apiURL := fmt.Sprintf("%s%s", c.internalEndpoint, endpoint)

	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}

	req, err := http.NewRequest(method, apiURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	if accessToken != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call Casdoor API: %w", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	return responseBody, nil
}

// GetOrganizations retrieves organizations from Casdoor
func (c *CasdoorClient) GetOrganizations(accessToken string) ([]byte, error) {
	return c.CallCasdoorAPI("GET", "/api/get-organizations", accessToken, nil)
}

// GetUsers retrieves users from Casdoor
func (c *CasdoorClient) GetUsers(accessToken string) ([]byte, error) {
	return c.CallCasdoorAPI("GET", "/api/get-users", accessToken, nil)
}

// SearchUsers searches users in Casdoor by username or email keyword
func (c *CasdoorClient) SearchUsers(accessToken, keyword string) ([]CasdoorUser, error) {
	apiURL := fmt.Sprintf("%s/api/get-users?owner=%s", c.internalEndpoint, url.QueryEscape(c.organization))

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Use client credentials (Basic Auth) for admin API access,
	// as /api/get-users requires admin privileges that normal user tokens lack.
	req.SetBasicAuth(c.clientID, c.secret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to search users: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	log.Printf("[SearchUsers] GET %s -> status=%d, body=%s", apiURL, resp.StatusCode, string(body))

	// Casdoor returns wrapped response: {"status": "ok", "data": [...], "msg": ""}
	var wrappedResp struct {
		Status string        `json:"status"`
		Data   []CasdoorUser `json:"data"`
		Msg    string        `json:"msg"`
	}
	if err := json.Unmarshal(body, &wrappedResp); err != nil {
		// Try direct array format (fallback)
		var users []CasdoorUser
		if err2 := json.Unmarshal(body, &users); err2 != nil {
			return nil, fmt.Errorf("failed to unmarshal users: %w", err)
		}
		wrappedResp.Data = users
	}

	if keyword == "" {
		return wrappedResp.Data, nil
	}

	lower := strings.ToLower(keyword)
	var matched []CasdoorUser
	for _, u := range wrappedResp.Data {
		if strings.Contains(strings.ToLower(u.Name), lower) ||
			strings.Contains(strings.ToLower(u.Email), lower) {
			matched = append(matched, u)
		}
	}
	return matched, nil
}

// Logout calls Casdoor SSO logout to invalidate the user's session and expire tokens.
// If logoutAll is true, all sessions and tokens for the user are invalidated;
// otherwise only the current session is ended.
func (c *CasdoorClient) Logout(accessToken string, logoutAll bool) error {
	logoutURL := fmt.Sprintf("%s/api/sso-logout?logoutAll=%t", c.internalEndpoint, logoutAll)

	req, err := http.NewRequest("POST", logoutURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create logout request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to call Casdoor logout: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Casdoor logout returned status %d: %s", resp.StatusCode, string(body))
	}

	// Check Casdoor response status
	var result struct {
		Status string `json:"status"`
		Msg    string `json:"msg"`
	}
	if err := json.Unmarshal(body, &result); err == nil && result.Status == "error" {
		return fmt.Errorf("Casdoor logout error: %s", result.Msg)
	}

	return nil
}

// GetGroups retrieves groups from Casdoor
func (c *CasdoorClient) GetGroups(accessToken string) ([]byte, error) {
	return c.CallCasdoorAPI("GET", "/api/get-groups", accessToken, nil)
}

// GetUserByID retrieves a user by ID (UUID) from Casdoor
func (c *CasdoorClient) GetUserByID(accessToken, userID string) (*CasdoorUser, error) {
	apiURL := fmt.Sprintf("%s/api/get-user?userId=%s", c.internalEndpoint, url.QueryEscape(userID))

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Use client credentials for admin API access, same as SearchUsers
	req.SetBasicAuth(c.clientID, c.secret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get user: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Casdoor returns wrapped response: {"status": "ok", "data": {...}, "msg": ""}
	var wrappedResp struct {
		Status string      `json:"status"`
		Data   CasdoorUser `json:"data"`
		Msg    string      `json:"msg"`
	}
	if err := json.Unmarshal(body, &wrappedResp); err != nil {
		// Try direct object format (fallback)
		var user CasdoorUser
		if err2 := json.Unmarshal(body, &user); err2 != nil {
			return nil, fmt.Errorf("failed to unmarshal user: %w", err)
		}
		return &user, nil
	}

	if wrappedResp.Data.Name == "" && wrappedResp.Data.Id == "" {
		return nil, fmt.Errorf("user not found: %s", userID)
	}

	return &wrappedResp.Data, nil
}

// GetUsersByIDs retrieves multiple users by their IDs from Casdoor.
// Uses per-ID lookups via /api/get-user to avoid dependency on organization config.
func (c *CasdoorClient) GetUsersByIDs(accessToken string, userIDs []string) (map[string]*CasdoorUser, error) {
	userMap := make(map[string]*CasdoorUser, len(userIDs))
	for _, id := range userIDs {
		user, err := c.GetUserByID(accessToken, id)
		if err != nil {
			log.Printf("[WARN] GetUsersByIDs: failed to get user %s: %v", id, err)
			continue
		}
		userMap[id] = user
	}
	return userMap, nil
}
