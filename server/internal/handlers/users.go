package handlers

import (
	"context"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	// imported for swag to resolve casdoor.CasdoorUser in godoc annotations
	"github.com/costrict/costrict-web/server/internal/casdoor"
	_ "github.com/costrict/costrict-web/server/internal/casdoor"
	userpkg "github.com/costrict/costrict-web/server/internal/user"

	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// In-memory user name cache (to be replaced with Redis later)
// ---------------------------------------------------------------------------

type userNameEntry struct {
	name      string
	expiresAt time.Time
}

var (
	userNameCache   = make(map[string]userNameEntry)
	userNameCacheMu sync.RWMutex
	userNameCacheTTL = 10 * time.Minute
)

// getCachedUserNames returns a map of userID -> displayName for the given IDs.
// Cache hits are served from memory; misses are fetched from Casdoor in a single
// batch call and then cached.
func getCachedUserNames(accessToken string, ids []string) map[string]string {
	now := time.Now()
	result := make(map[string]string, len(ids))
	var missIDs []string

	// --- read from cache ---
	userNameCacheMu.RLock()
	for _, id := range ids {
		if entry, ok := userNameCache[id]; ok && now.Before(entry.expiresAt) {
			result[id] = entry.name
		} else {
			missIDs = append(missIDs, id)
		}
	}
	userNameCacheMu.RUnlock()

	if len(missIDs) == 0 {
		return result
	}

	// --- fetch misses from Casdoor ---
	if CasdoorClient == nil {
		return result
	}
	userMap, err := CasdoorClient.GetUsersByIDs(accessToken, missIDs)
	if err != nil {
		log.Printf("[WARN] getCachedUserNames: Casdoor lookup failed: %v", err)
		return result
	}

	// --- populate cache + result ---
	userNameCacheMu.Lock()
	expiry := now.Add(userNameCacheTTL)
	for _, id := range missIDs {
		displayName := id // fallback to ID itself
		if user, ok := userMap[id]; ok {
			displayName = user.Name
			if user.PreferredUsername != "" {
				displayName = user.PreferredUsername
			}
		}
		userNameCache[id] = userNameEntry{name: displayName, expiresAt: expiry}
		result[id] = displayName
	}
	userNameCacheMu.Unlock()

	return result
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// GetUserNames godoc
// @Summary      Batch resolve user display names
// @Description  Given a comma-separated list of user IDs, return a map of id -> displayName. Results are served from an in-memory cache when possible.
// @Tags         users
// @Produce      json
// @Param        ids   query     string  true  "Comma-separated user IDs (max 50)"
// @Success      200   {object}  object{names=map[string]string}
// @Failure      400   {object}  object{error=string}
// @Router       /users/names [get]
func GetUserNames(c *gin.Context) {
	raw := c.Query("ids")
	if raw == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "ids parameter is required"})
		return
	}

	ids := strings.Split(raw, ",")
	// Deduplicate and cap at 50
	seen := make(map[string]bool, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	if len(unique) > 50 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "too many IDs, max 50"})
		return
	}

	names := make(map[string]string, len(unique))
	remaining := unique

	if UserModule != nil && UserModule.CachedService != nil {
		userMap, err := UserModule.CachedService.GetUsersByIDs(c.Request.Context(), unique)
		if err == nil {
			remaining = remaining[:0]
			for _, id := range unique {
				if user, ok := userMap[id]; ok && user != nil {
					displayName := user.Username
					if user.DisplayName != nil && *user.DisplayName != "" {
						displayName = *user.DisplayName
					}
					names[id] = displayName
				} else {
					remaining = append(remaining, id)
				}
			}
		}
	}

	if len(remaining) > 0 {
		// Try to get access token from context (optional auth)
		token, _ := c.Get("accessToken")
		tokenStr, _ := token.(string)
		fallbackNames := getCachedUserNames(tokenStr, remaining)
		for k, v := range fallbackNames {
			names[k] = v
		}
	}

	c.JSON(http.StatusOK, gin.H{"names": names})
}

// SearchUsers godoc
// @Summary      Search users
// @Description  Search users by username or email keyword (requires authentication)
// @Tags         users
// @Produce      json
// @Param        q     query     string  true  "Search keyword"
// @Success      200   {object}  object{users=[]casdoor.CasdoorUser}
// @Failure      401   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /users/search [get]
func SearchUsers(c *gin.Context) {
	token, exists := c.Get("accessToken")
	if !exists || token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	keyword := c.Query("q")
	limit := 20

	if UserModule != nil && UserModule.Service != nil {
		users, err := UserModule.Service.SearchUsers(keyword, limit)
		if err == nil && len(users) > 0 {
			c.JSON(http.StatusOK, gin.H{"users": users})
			return
		}
	}

	client := CasdoorClient
	users, err := client.SearchUsers(token.(string), keyword)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to search users"})
		return
	}

	if len(users) > limit {
		users = users[:limit]
	}

	if UserModule != nil && UserModule.Service != nil {
		go backfillUsers(context.Background(), users)
	}

	c.JSON(http.StatusOK, gin.H{"users": users})
}

func backfillUsers(ctx context.Context, users []casdoor.CasdoorUser) {
	if UserModule == nil || UserModule.Service == nil {
		return
	}

	for _, u := range users {
		claims := &userpkg.JWTClaims{
			ID:                u.Id,
			Sub:               u.Sub,
			Name:              u.Name,
			PreferredUsername: u.PreferredUsername,
			Email:             u.Email,
			Picture:           u.Picture,
			Owner:             u.Owner,
		}
		_, _ = UserModule.Service.GetOrCreateUser(claims)
	}
}
