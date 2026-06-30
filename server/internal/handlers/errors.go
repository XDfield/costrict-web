package handlers

import "github.com/gin-gonic/gin"

// respondError records err on the gin context so the ErrorLogger middleware
// surfaces the real cause in app.log / error.log, and returns an HTTP response
// with the given status code and public message (which does not leak internal
// details to the client).
func respondError(c *gin.Context, err error, statusCode int, publicMsg string) {
	_ = c.Error(err)
	c.JSON(statusCode, gin.H{"error": publicMsg})
}
