package middleware

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
)

const UserIDKey = "userId"
const UserNameKey = "userName"
const InternalSecretHeader = "X-Internal-Secret"
const SystemTokenHeader = "X-System-Token"

type SubjectResolver func(claims AuthClaims) (subjectID string, preferredUsername string, err error)

type AuthClaims struct {
	ID                string
	Sub               string
	UniversalID       string
	Name              string
	PreferredUsername string
	Email             string
}

var subjectResolver SubjectResolver

func SetSubjectResolver(resolver SubjectResolver) {
	subjectResolver = resolver
}

// InternalAuth validates requests from internal services (gateway, etc.) using a shared secret.
// If secret is empty, all requests are rejected to prevent misconfiguration.
func InternalAuth(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if secret == "" {
			logger.Error("[InternalAuth] INTERNAL_SECRET not configured, rejecting request")
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Internal API not available"})
			return
		}

		provided := c.GetHeader(InternalSecretHeader)
		if provided == "" || provided != secret {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Invalid internal secret"})
			return
		}

		c.Next()
	}
}

func SystemTokenAuth(token string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if token == "" {
			logger.Error("[SystemTokenAuth] SYSTEM_TOKEN not configured, rejecting request")
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "System API not available"})
			return
		}

		provided := c.GetHeader(SystemTokenHeader)
		if provided == "" || provided != token {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "Invalid system token"})
			return
		}

		c.Set(UserIDKey, "system")
		c.Next()
	}
}

// ExtractToken extracts the access token from the Authorization header or auth_token cookie.
func ExtractToken(c *gin.Context) string {
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	if cookie, err := c.Cookie("auth_token"); err == nil && cookie != "" {
		return cookie
	}
	return ""
}

func OptionalAuth(casdoorEndpoint string, jwks *JWKSProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ExtractToken(c)
		if token == "" {
			c.Next()
			return
		}

		userInfo, err := parseJWTToken(token, jwks)
		if err != nil {
			// Fallback to Casdoor API verification
			userInfo, err = fetchUserInfo(casdoorEndpoint, token)
			if err != nil {
				logger.Warn("[OptionalAuth] token validation failed: %v, endpoint=%s", err, casdoorEndpoint)
				c.Next()
				return
			}
		}

		setAuthContext(c, userInfo)
		c.Set("accessToken", token)
		c.Next()
	}
}

func RequireAuth(casdoorEndpoint string, jwks *JWKSProvider) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := ExtractToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		userInfo, err := parseJWTToken(token, jwks)
		if err != nil {
			// Fallback to Casdoor API verification
			userInfo, err = fetchUserInfo(casdoorEndpoint, token)
			if err != nil {
				logger.Warn("[RequireAuth] token validation failed: %v, endpoint=%s", err, casdoorEndpoint)
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
				return
			}
		}

		setAuthContext(c, userInfo)
		c.Set("accessToken", token)
		c.Next()
	}
}

type casdoorUserInfo struct {
	ID               string `json:"id"`
	Sub              string `json:"sub"`
	UniversalID      string `json:"universal_id"`
	Name             string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Email            string `json:"email"`
}

type casdoorUserinfoResponse struct {
	Status      string `json:"status"`
	Msg         string `json:"msg"`
	ID          string `json:"id"`
	Sub         string `json:"sub"`
	UniversalID string `json:"universal_id"`
	Name        string `json:"name"`
	Email       string `json:"email"`
}

// parseJWTToken verifies and parses a Casdoor JWT token using JWKS public keys.
// If jwks is nil or key retrieval fails, returns an error so the caller can fall back.
func parseJWTToken(tokenString string, jwks *JWKSProvider) (*casdoorUserInfo, error) {
	if jwks == nil {
		return nil, fmt.Errorf("JWKS provider not configured")
	}

	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (interface{}, error) {
		// Ensure the signing method is RSA
		if _, ok := token.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		kid, _ := token.Header["kid"].(string)
		return jwks.GetKey(kid)
	}, jwt.WithValidMethods([]string{"RS256"}))

	if err != nil {
		return nil, fmt.Errorf("JWT verification failed: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	// Extract sub (universal_id in Casdoor)
	sub, _ := claims["id"].(string)
	if sub == "" {
		sub, _ = claims["sub"].(string)
	}
	if sub == "" {
		sub, _ = claims["universal_id"].(string)
	}
	if sub == "" {
		return nil, fmt.Errorf("no id, sub or universal_id in token")
	}

	name, _ := claims["name"].(string)
	preferredUsername, _ := claims["preferred_username"].(string)
	if preferredUsername == "" {
		preferredUsername = name
	}
	email, _ := claims["email"].(string)

	return &casdoorUserInfo{
		ID:               strClaim(claims, "id"),
		Sub:              sub,
		UniversalID:      strClaim(claims, "universal_id"),
		Name:             name,
		PreferredUsername: preferredUsername,
		Email:            email,
	}, nil
}

func fetchUserInfo(endpoint, token string) (*casdoorUserInfo, error) {
	url := endpoint + "/api/userinfo"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s failed: %w", url, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		logger.Warn("[fetchUserInfo] casdoor returned %d: %s, url=%s", resp.StatusCode, string(body), url)
		return nil, fmt.Errorf("casdoor returned status %d", resp.StatusCode)
	}

	var casdoorResp casdoorUserinfoResponse
	if err := json.Unmarshal(body, &casdoorResp); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}

	// Check if Casdoor returned an error
	if casdoorResp.Status == "error" {
		return nil, fmt.Errorf("casdoor error: %s", casdoorResp.Msg)
	}

	if casdoorResp.Sub == "" {
		return nil, fmt.Errorf("no sub in response")
	}

	return &casdoorUserInfo{
		ID:               casdoorResp.ID,
		Sub:              casdoorResp.Sub,
		UniversalID:      casdoorResp.UniversalID,
		Name:             casdoorResp.Name,
		PreferredUsername: casdoorResp.Name,
		Email:            casdoorResp.Email,
	}, nil
}

func setAuthContext(c *gin.Context, userInfo *casdoorUserInfo) {
	userID := userInfo.Sub
	userName := userInfo.PreferredUsername
	if subjectResolver != nil {
		resolvedID, resolvedName, err := subjectResolver(AuthClaims{
			ID:                userInfo.ID,
			Sub:               userInfo.Sub,
			UniversalID:       userInfo.UniversalID,
			Name:              userInfo.Name,
			PreferredUsername: userInfo.PreferredUsername,
			Email:             userInfo.Email,
		})
		if err == nil {
			if resolvedID != "" {
				userID = resolvedID
			}
			if resolvedName != "" {
				userName = resolvedName
			}
		}
	}
	c.Set(UserIDKey, userID)
	c.Set(UserNameKey, userName)
}

func strClaim(claims jwt.MapClaims, key string) string {
	if v, ok := claims[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
