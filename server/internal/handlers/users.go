package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

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
	callerIDVal, exists := c.Get("userId")
	if !exists || callerIDVal == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	keyword := c.Query("q")
	token := extractBearerToken(c)

	client := CasdoorClient
	users, err := client.SearchUsers(token, keyword)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to search users"})
		return
	}

	limit := 20
	if len(users) > limit {
		users = users[:limit]
	}

	c.JSON(http.StatusOK, gin.H{"users": users})
}

func extractBearerToken(c *gin.Context) string {
	auth := c.GetHeader("Authorization")
	if len(auth) > 7 && auth[:7] == "Bearer " {
		return auth[7:]
	}
	if cookie, err := c.Cookie("auth_token"); err == nil {
		return cookie
	}
	return ""
}
