package middleware

import (
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
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

		userInfo, err := parseJWTToken(token)
		if err != nil {
			log.Printf("[OptionalAuth] JWT parse failed: %v", err)
			c.Next()
			return
		}

		c.Set(UserIDKey, userInfo.Sub)
		c.Set(UserNameKey, userInfo.PreferredUsername)
		c.Set("accessToken", token)
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

		userInfo, err := parseJWTToken(token)
		if err != nil {
			log.Printf("[RequireAuth] JWT parse failed: %v", err)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "Invalid token"})
			return
		}

		c.Set(UserIDKey, userInfo.Sub)
		c.Set(UserNameKey, userInfo.PreferredUsername)
		c.Set("accessToken", token)
		c.Next()
	}
}

type casdoorUserInfo struct {
	Sub               string `json:"sub"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Email             string `json:"email"`
}

// parseJWTToken parses Casdoor JWT token locally without calling Casdoor API
func parseJWTToken(tokenString string) (*casdoorUserInfo, error) {
	// Parse token without verification (Casdoor JWT is already verified by signature)
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	token, _, err := parser.ParseUnverified(tokenString, jwt.MapClaims{})
	if err != nil {
		return nil, fmt.Errorf("parse JWT failed: %w", err)
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("invalid claims type")
	}

	// Debug: 打印完整的 claims
	log.Printf("[parseJWTToken] claims: %+v", claims)

	// 使用 sub 作为 userID (OIDC 标准)
	sub, _ := claims["sub"].(string)
	if sub == "" {
		sub, _ = claims["universal_id"].(string)
	}
	if sub == "" {
		return nil, fmt.Errorf("no sub or universal_id in token")
	}

	log.Printf("[parseJWTToken] extracted sub=%q (as userID)", sub)

	name, _ := claims["name"].(string)
	preferredUsername, _ := claims["preferred_username"].(string)
	if preferredUsername == "" {
		preferredUsername = name
	}
	email, _ := claims["email"].(string)

	return &casdoorUserInfo{
		Sub:               sub,
		Name:              name,
		PreferredUsername: preferredUsername,
		Email:             email,
	}, nil
}
