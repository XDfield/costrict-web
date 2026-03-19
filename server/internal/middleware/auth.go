package middleware

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const UserIDKey = "userId"
const UserNameKey = "userName"

func extractToken(c *gin.Context) string {
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	if cookie, err := c.Cookie("auth_token"); err == nil && cookie != "" {
		return cookie
	}
	return ""
}

func OptionalAuth(casdoorEndpoint string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c)
		if token == "" {
			c.Next()
			return
		}

		userInfo, err := fetchUserInfo(casdoorEndpoint, token)
		if err != nil {
			log.Printf("[OptionalAuth] fetchUserInfo failed: %v, endpoint=%s", err, casdoorEndpoint)
			c.Next()
			return
		}

		c.Set(UserIDKey, userInfo.Sub)
		c.Set(UserNameKey, userInfo.PreferredUsername)
		c.Next()
	}
}

func RequireAuth(casdoorEndpoint string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := extractToken(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		userInfo, err := fetchUserInfo(casdoorEndpoint, token)
		if err != nil {
			log.Printf("[RequireAuth] fetchUserInfo failed: %v, endpoint=%s", err, casdoorEndpoint)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			return
		}

		c.Set(UserIDKey, userInfo.Sub)
		c.Set(UserNameKey, userInfo.PreferredUsername)
		c.Next()
	}
}

type casdoorUserInfo struct {
	Sub               string `json:"sub"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
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

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[fetchUserInfo] casdoor returned %d: %s, url=%s", resp.StatusCode, string(body), url)
		return nil, fmt.Errorf("casdoor returned status %d", resp.StatusCode)
	}

	var info casdoorUserInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return nil, fmt.Errorf("decode response failed: %w", err)
	}
	return &info, nil
}
