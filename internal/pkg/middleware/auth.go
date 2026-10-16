package middleware

import (
	"net/http"
	"strings"

	"travel-app-backend/internal/pkg/auth"

	"github.com/gin-gonic/gin"
)

const (
	// ContextUserID is the authenticated user id set by BearerAuth.
	ContextUserID = "auth.userID"
	// ContextUserEmail is the authenticated user email set by BearerAuth.
	ContextUserEmail = "auth.userEmail"
)

// BearerAuth validates the Authorization: Bearer <jwt> header and injects
// the authenticated user into the Gin context. Blocks with 401 otherwise.
func BearerAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			abortUnauthorized(c, "missing Authorization header")
			return
		}
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			abortUnauthorized(c, "Authorization header must be: Bearer <token>")
			return
		}
		claims, err := auth.ValidateToken(token)
		if err != nil {
			abortUnauthorized(c, "invalid or expired token")
			return
		}
		c.Set(ContextUserID, claims.UserID)
		c.Set(ContextUserEmail, claims.Email)
		c.Next()
	}
}

// UserID returns the authenticated user id from context.
func UserID(c *gin.Context) (int64, bool) {
	v, ok := c.Get(ContextUserID)
	if !ok {
		return 0, false
	}
	id, ok := v.(int64)
	return id, ok
}

func abortUnauthorized(c *gin.Context, msg string) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
		"success": false,
		"error":   msg,
		"code":    http.StatusUnauthorized,
	})
}
