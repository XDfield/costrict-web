package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseConfig_Complete(t *testing.T) {
	raw := map[string]interface{}{
		"authorization_url": "https://github.com/login/oauth/authorize",
		"token_url":         "https://github.com/login/oauth/access_token",
		"userinfo_url":      "https://api.github.com/user",
		"client_id":         "id",
		"client_secret":     "secret",
		"scopes":            []interface{}{"read:user", "user:email"},
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.ClientID != "id" || cfg.ClientSecret != "secret" {
		t.Errorf("credentials: %+v", cfg)
	}
	if len(cfg.Scopes) != 2 || cfg.Scopes[0] != "read:user" {
		t.Errorf("scopes: %v", cfg.Scopes)
	}
}

func TestParseConfig_MissingFields(t *testing.T) {
	if _, err := ParseConfig(map[string]interface{}{}); err == nil {
		t.Error("expected error for empty config")
	}
	if _, err := ParseConfig(map[string]interface{}{
		"authorization_url": "x", "token_url": "y", "userinfo_url": "z",
	}); err == nil {
		t.Error("expected error for missing client_id/secret")
	}
}

func TestParseConfig_ScopesAsString(t *testing.T) {
	cfg, err := ParseConfig(map[string]interface{}{
		"authorization_url": "x", "token_url": "y", "userinfo_url": "z",
		"client_id": "id", "client_secret": "sec",
		"scopes": "openid email",
	})
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(cfg.Scopes) != 2 || cfg.Scopes[0] != "openid" {
		t.Errorf("scopes: %v", cfg.Scopes)
	}
}

func TestAuthorizationURL(t *testing.T) {
	c := NewClient()
	cfg := Config{
		AuthorizationURL: "https://github.com/login/oauth/authorize",
		ClientID:         "cid",
		Scopes:           []string{"read:user"},
	}
	u, err := c.AuthorizationURL(cfg, "https://app/cb", "state123")
	if err != nil {
		t.Fatalf("AuthorizationURL: %v", err)
	}
	if !strings.HasPrefix(u, "https://github.com/login/oauth/authorize?") {
		t.Errorf("url prefix wrong: %s", u)
	}
	if !strings.Contains(u, "client_id=cid") {
		t.Errorf("client_id missing: %s", u)
	}
	if !strings.Contains(u, "state=state123") {
		t.Errorf("state missing: %s", u)
	}
	if !strings.Contains(u, "scope=read") {
		t.Errorf("scope missing: %s", u)
	}
}

func TestExchangeCodeForToken_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method: %s", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if r.FormValue("code") != "abc" {
			t.Errorf("code form value: %q", r.FormValue("code"))
		}
		if r.FormValue("client_secret") != "shh" {
			t.Errorf("client_secret form value: %q", r.FormValue("client_secret"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TokenResponse{AccessToken: "atok", TokenType: "bearer"})
	}))
	defer srv.Close()

	c := NewClient()
	cfg := Config{TokenURL: srv.URL, ClientID: "cid", ClientSecret: "shh"}
	tok, err := c.ExchangeCodeForToken(context.Background(), cfg, "abc", "https://app/cb")
	if err != nil {
		t.Fatalf("ExchangeCodeForToken: %v", err)
	}
	if tok.AccessToken != "atok" {
		t.Errorf("access token: %s", tok.AccessToken)
	}
}

func TestExchangeCodeForToken_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()

	c := NewClient()
	cfg := Config{TokenURL: srv.URL, ClientID: "cid", ClientSecret: "s"}
	_, err := c.ExchangeCodeForToken(context.Background(), cfg, "abc", "https://app/cb")
	if err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Errorf("expected token exchange error, got %v", err)
	}
}

func TestFetchUserInfo_Normalized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer mytok" {
			t.Errorf("auth header: %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		// GitHub-style response
		_, _ = w.Write([]byte(`{
			"id": 12345,
			"login": "octocat",
			"name": "The Octocat",
			"email": "octo@github.com",
			"avatar_url": "https://github.com/images/error/octocat_happy.gif"
		}`))
	}))
	defer srv.Close()

	c := NewClient()
	cfg := Config{UserInfoURL: srv.URL}
	profile, err := c.FetchUserInfo(context.Background(), cfg, "mytok", FieldMap{})
	if err != nil {
		t.Fatalf("FetchUserInfo: %v", err)
	}
	if profile.Subject != "12345" { // GitHub id is numeric, normalized to string
		t.Errorf("subject: %q", profile.Subject)
	}
	if profile.Username != "octocat" {
		t.Errorf("username: %q", profile.Username)
	}
	if profile.Email != "octo@github.com" {
		t.Errorf("email: %q", profile.Email)
	}
}

func TestFetchUserInfo_WithFieldMap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"open_id":"ou-xyz","name":"Feishu User","email":"u@f.com"}`))
	}))
	defer srv.Close()

	c := NewClient()
	cfg := Config{UserInfoURL: srv.URL}
	profile, err := c.FetchUserInfo(context.Background(), cfg, "tok", FieldMap{
		Subject: "open_id",
	})
	if err != nil {
		t.Fatalf("FetchUserInfo: %v", err)
	}
	if profile.Subject != "ou-xyz" {
		t.Errorf("subject (feishu open_id): %q", profile.Subject)
	}
}
