package casdoor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/costrict/costrict-web/server/internal/config"
)

type CasdoorClient struct {
	endpoint   string
	clientID   string
	secret     string
	callbackURL string
}

type CasdoorUser struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Avatar   string `json:"avatar"`
	CreatedAt string `json:"created_at"`
}

type CasdoorTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
	Scope       string `json:"scope"`
}

type CasdoorUserInfoResponse struct {
	User CasdoorUser `json:"user"`
}

func NewClient(cfg *config.CasdoorConfig) *CasdoorClient {
	return &CasdoorClient{
		endpoint:   cfg.Endpoint,
		clientID:   cfg.ClientID,
		secret:     cfg.Secret,
		callbackURL: cfg.CallbackURL,
	}
}

// GetLoginURL returns = Casdoor login URL
func (c *CasdoorClient) GetLoginURL(state string) string {
	params := url.Values{}
	params.Set("client_id", c.clientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", c.callbackURL)
	params.Set("scope", "openid profile email")
	params.Set("state", state)

	return fmt.Sprintf("%s/login/oauth/authorize?%s", c.endpoint, params.Encode())
}

// ExchangeCodeForToken exchanges = authorization code for an access token
func (c *CasdoorClient) ExchangeCodeForToken(code string) (*CasdoorTokenResponse, error) {
	tokenURL := fmt.Sprintf("%s/api/login/oauth/access_token", c.endpoint)

	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("client_id", c.clientID)
	data.Set("client_secret", c.secret)
	data.Set("code", code)
	data.Set("redirect_uri", c.callbackURL)

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

// GetUserInfo retrieves user information from Casdoor
func (c *CasdoorClient) GetUserInfo(accessToken string) (*CasdoorUserInfoResponse, error) {
	userInfoURL := fmt.Sprintf("%s/api/userinfo", c.endpoint)

	req, err := http.NewRequest("GET", userInfoURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to get user info: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var userInfoResp CasdoorUserInfoResponse
	if err := json.Unmarshal(body, &userInfoResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal user info response: %w", err)
	}

	return &userInfoResp, nil
}

// CallCasdoorAPI makes a generic API call to Casdoor
func (c *CasdoorClient) CallCasdoorAPI(method, endpoint string, accessToken string, body interface{}) ([]byte, error) {
	apiURL := fmt.Sprintf("%s%s", c.endpoint, endpoint)

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

// GetGroups retrieves groups from Casdoor
func (c *CasdoorClient) GetGroups(accessToken string) ([]byte, error) {
	return c.CallCasdoorAPI("GET", "/api/get-groups", accessToken, nil)
}
