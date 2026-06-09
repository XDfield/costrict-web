package middleware

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/gin-gonic/gin"
)

type ctxKey string

const (
	CtxUserID   ctxKey = "user_id"
	CtxUserName ctxKey = "user_name"
	CtxUserSub  ctxKey = "user_sub"
)

type jwtPayload struct {
	Sub             string `json:"sub"`
	UniversalID     string `json:"universal_id"`
	PreferredName   string `json:"preferred_username"`
}

func JWTDecode() gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if auth == "" {
			c.Set(string(CtxUserID), "")
			c.Set(string(CtxUserName), "")
			c.Set(string(CtxUserSub), "")
			c.Next()
			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")
		if token == auth {
			c.Set(string(CtxUserID), "")
			c.Set(string(CtxUserName), "")
			c.Set(string(CtxUserSub), "")
			c.Next()
			return
		}

		userID, userName, userSub := decodeJWTPayload(token)
		c.Set(string(CtxUserID), userID)
		c.Set(string(CtxUserName), userName)
		c.Set(string(CtxUserSub), userSub)
		c.Next()
	}
}

func decodeJWTPayload(token string) (userID, userName, userSub string) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", "", ""
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", ""
	}

	var p jwtPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", "", ""
	}

	return p.UniversalID, p.PreferredName, p.Sub
}

func GetUserID(ctx context.Context) string {
	if v, ok := ctx.Value(CtxUserID).(string); ok {
		return v
	}
	return ""
}

func GetUserName(ctx context.Context) string {
	if v, ok := ctx.Value(CtxUserName).(string); ok {
		return v
	}
	return ""
}

func GetUserSub(ctx context.Context) string {
	if v, ok := ctx.Value(CtxUserSub).(string); ok {
		return v
	}
	return ""
}
