package internal

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// ExtractDeviceToken tests
// ---------------------------------------------------------------------------

func TestExtractDeviceToken_FromQueryParam(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/tunnel?token=abc123", nil)
	got := ExtractDeviceToken(req)
	if got != "abc123" {
		t.Errorf("expected %q, got %q", "abc123", got)
	}
}

func TestExtractDeviceToken_FromBearerHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/tunnel", nil)
	req.Header.Set("Authorization", "Bearer header-token-xyz")
	got := ExtractDeviceToken(req)
	if got != "header-token-xyz" {
		t.Errorf("expected %q, got %q", "header-token-xyz", got)
	}
}

func TestExtractDeviceToken_QueryParamPriorityOverHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/tunnel?token=from-query", nil)
	req.Header.Set("Authorization", "Bearer from-header")
	got := ExtractDeviceToken(req)
	if got != "from-query" {
		t.Errorf("expected query-param token %q, got %q", "from-query", got)
	}
}

func TestExtractDeviceToken_EmptyWhenNeitherPresent(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/tunnel", nil)
	got := ExtractDeviceToken(req)
	if got != "" {
		t.Errorf("expected empty string, got %q", got)
	}
}

func TestExtractDeviceToken_URLEncodedQueryParam(t *testing.T) {
	// net/http automatically decodes percent-encoded query parameters via
	// r.URL.Query().Get(), so a token containing special characters like
	// spaces (encoded as %20) or equals signs (%3D) should be decoded.
	req := httptest.NewRequest(http.MethodGet, "/tunnel?token=hello%20world%3Dfoo", nil)
	got := ExtractDeviceToken(req)
	expected := "hello world=foo"
	if got != expected {
		t.Errorf("expected %q, got %q", expected, got)
	}
}

func TestExtractDeviceToken_AuthorizationWithoutBearerPrefix(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/tunnel", nil)
	req.Header.Set("Authorization", "Basic abc123")
	got := ExtractDeviceToken(req)
	if got != "" {
		t.Errorf("expected empty string for non-Bearer auth, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// VerifyDeviceToken tests
// ---------------------------------------------------------------------------

// newVerifyServer creates an httptest.Server that mimics the server's
// /internal/gateway/device/verify-token endpoint. The handler parameter
// receives the decoded request body and the raw *http.Request so tests
// can inspect headers.
func newVerifyServer(handler func(body map[string]string, r *http.Request) (int, any)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/gateway/device/verify-token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		json.Unmarshal(raw, &body)

		statusCode, respBody := handler(body, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		json.NewEncoder(w).Encode(respBody)
	}))
}

func TestVerifyDeviceToken_ValidToken(t *testing.T) {
	server := newVerifyServer(func(body map[string]string, r *http.Request) (int, any) {
		return http.StatusOK, map[string]any{
			"valid":  true,
			"userID": "user-42",
		}
	})
	defer server.Close()

	userID, err := VerifyDeviceToken(server.URL, "secret", "device-1", "valid-token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if userID != "user-42" {
		t.Errorf("expected userID %q, got %q", "user-42", userID)
	}
}

func TestVerifyDeviceToken_InvalidToken(t *testing.T) {
	server := newVerifyServer(func(body map[string]string, r *http.Request) (int, any) {
		return http.StatusOK, map[string]any{
			"valid":  false,
			"userID": "",
		}
	})
	defer server.Close()

	_, err := VerifyDeviceToken(server.URL, "secret", "device-1", "bad-token")
	if err == nil {
		t.Fatal("expected error for invalid token, got nil")
	}
	if got := err.Error(); got != "invalid device token" {
		t.Errorf("expected error %q, got %q", "invalid device token", got)
	}
}

func TestVerifyDeviceToken_ServerReturnsNon200(t *testing.T) {
	server := newVerifyServer(func(body map[string]string, r *http.Request) (int, any) {
		return http.StatusInternalServerError, map[string]any{"error": "boom"}
	})
	defer server.Close()

	_, err := VerifyDeviceToken(server.URL, "secret", "device-1", "token")
	if err == nil {
		t.Fatal("expected error for non-200 status, got nil")
	}
	expected := "verify-token returned status 500"
	if got := err.Error(); got != expected {
		t.Errorf("expected error %q, got %q", expected, got)
	}
}

func TestVerifyDeviceToken_ServerUnreachable(t *testing.T) {
	// Use a URL that will definitely fail to connect.
	_, err := VerifyDeviceToken("http://127.0.0.1:1", "secret", "device-1", "token")
	if err == nil {
		t.Fatal("expected error for unreachable server, got nil")
	}
	// The error should mention the request failure.
	if got := err.Error(); len(got) == 0 {
		t.Error("expected non-empty error message")
	}
}

func TestVerifyDeviceToken_InternalSecretHeaderSent(t *testing.T) {
	var receivedSecret string
	server := newVerifyServer(func(body map[string]string, r *http.Request) (int, any) {
		receivedSecret = r.Header.Get(internalSecretHeader)
		return http.StatusOK, map[string]any{"valid": true, "userID": "u1"}
	})
	defer server.Close()

	_, err := VerifyDeviceToken(server.URL, "my-secret-123", "device-1", "tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedSecret != "my-secret-123" {
		t.Errorf("expected header %q=%q, got %q", internalSecretHeader, "my-secret-123", receivedSecret)
	}
}

func TestVerifyDeviceToken_InternalSecretHeaderOmittedWhenEmpty(t *testing.T) {
	var receivedSecret string
	var headerPresent bool
	server := newVerifyServer(func(body map[string]string, r *http.Request) (int, any) {
		receivedSecret = r.Header.Get(internalSecretHeader)
		_, headerPresent = r.Header[internalSecretHeader]
		return http.StatusOK, map[string]any{"valid": true, "userID": "u1"}
	})
	defer server.Close()

	_, err := VerifyDeviceToken(server.URL, "", "device-1", "tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if headerPresent {
		t.Errorf("expected %q header to be absent when secret is empty, but got %q", internalSecretHeader, receivedSecret)
	}
}

func TestVerifyDeviceToken_RequestBodyContainsDeviceIDAndToken(t *testing.T) {
	var receivedBody map[string]string
	server := newVerifyServer(func(body map[string]string, r *http.Request) (int, any) {
		receivedBody = body
		return http.StatusOK, map[string]any{"valid": true, "userID": "u1"}
	})
	defer server.Close()

	_, err := VerifyDeviceToken(server.URL, "s", "dev-99", "tok-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedBody["deviceID"] != "dev-99" {
		t.Errorf("expected deviceID=%q, got %q", "dev-99", receivedBody["deviceID"])
	}
	if receivedBody["token"] != "tok-abc" {
		t.Errorf("expected token=%q, got %q", "tok-abc", receivedBody["token"])
	}
}

func TestVerifyDeviceToken_DeviceIDMismatchServerReturnsInvalid(t *testing.T) {
	// Simulate the server-side check: the token is valid but for a different device,
	// so the server responds with {valid: false}.
	server := newVerifyServer(func(body map[string]string, r *http.Request) (int, any) {
		return http.StatusOK, map[string]any{
			"valid":  false,
			"userID": "",
		}
	})
	defer server.Close()

	_, err := VerifyDeviceToken(server.URL, "secret", "wrong-device", "token")
	if err == nil {
		t.Fatal("expected error for device mismatch (valid=false), got nil")
	}
	if got := err.Error(); got != "invalid device token" {
		t.Errorf("expected error %q, got %q", "invalid device token", got)
	}
}

// ---------------------------------------------------------------------------
// InternalSecretAuth middleware tests
// ---------------------------------------------------------------------------

func init() {
	gin.SetMode(gin.TestMode)
}

// setupMiddlewareTest creates a gin engine with the InternalSecretAuth
// middleware guarding a simple 200 handler at GET /test.
func setupMiddlewareTest(secret string) *gin.Engine {
	r := gin.New()
	r.GET("/test", InternalSecretAuth(secret), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	return r
}

func TestInternalSecretAuth_EmptySecretRejectsAll(t *testing.T) {
	router := setupMiddlewareTest("")

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(internalSecretHeader, "any-value")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected status %d, got %d", http.StatusForbidden, w.Code)
	}

	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "internal API not available" {
		t.Errorf("expected error %q, got %q", "internal API not available", body["error"])
	}
}

func TestInternalSecretAuth_MissingHeaderRejects(t *testing.T) {
	router := setupMiddlewareTest("correct-secret")

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	// No X-Internal-Secret header set.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected status %d, got %d", http.StatusForbidden, w.Code)
	}

	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "invalid internal secret" {
		t.Errorf("expected error %q, got %q", "invalid internal secret", body["error"])
	}
}

func TestInternalSecretAuth_WrongSecretRejects(t *testing.T) {
	router := setupMiddlewareTest("correct-secret")

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(internalSecretHeader, "wrong-secret")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected status %d, got %d", http.StatusForbidden, w.Code)
	}

	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["error"] != "invalid internal secret" {
		t.Errorf("expected error %q, got %q", "invalid internal secret", body["error"])
	}
}

func TestInternalSecretAuth_CorrectSecretPasses(t *testing.T) {
	router := setupMiddlewareTest("correct-secret")

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(internalSecretHeader, "correct-secret")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status %d, got %d", http.StatusOK, w.Code)
	}

	var body map[string]string
	json.Unmarshal(w.Body.Bytes(), &body)
	if body["status"] != "ok" {
		t.Errorf("expected body status %q, got %q", "ok", body["status"])
	}
}
