package middleware

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func makeJWT(sub, universalID, preferredName string) string {
	payload, _ := json.Marshal(jwtPayload{
		Sub:           sub,
		UniversalID:   universalID,
		PreferredName: preferredName,
	})
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + body + ".fakesig"
}

func setupRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(JWTDecode())
	r.GET("/test", func(c *gin.Context) {
		c.JSON(200, gin.H{
			"user_id":   c.GetString(string(CtxUserID)),
			"user_name": c.GetString(string(CtxUserName)),
			"user_sub":  c.GetString(string(CtxUserSub)),
		})
	})
	return r
}

func TestJWTDecode_ValidToken(t *testing.T) {
	r := setupRouter()
	token := makeJWT("org/alice", "uid-123", "Alice")

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !contains(body, "uid-123") {
		t.Errorf("expected user_id uid-123, got %s", body)
	}
	if !contains(body, "Alice") {
		t.Errorf("expected user_name Alice, got %s", body)
	}
	if !contains(body, "org/alice") {
		t.Errorf("expected user_sub org/alice, got %s", body)
	}
}

func TestJWTDecode_NoAuth(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	r.ServeHTTP(w, req)

	body := w.Body.String()
	if !contains(body, `"user_id":""`) {
		t.Errorf("expected empty user_id, got %s", body)
	}
}

func TestJWTDecode_InvalidBase64(t *testing.T) {
	r := setupRouter()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer not.valid.jwt!!!")
	r.ServeHTTP(w, req)

	body := w.Body.String()
	if !contains(body, `"user_id":""`) {
		t.Errorf("expected empty user_id for invalid token, got %s", body)
	}
}

func TestJWTDecode_MissingFields(t *testing.T) {
	r := setupRouter()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"org/bob"}`))
	token := header + "." + payload + ".sig"

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	r.ServeHTTP(w, req)

	body := w.Body.String()
	if !contains(body, `"user_id":""`) {
		t.Errorf("expected empty user_id when universal_id missing, got %s", body)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
