package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ErrorResponse is the centralized error body for all failed requests.
type ErrorResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Code    int    `json:"code"`
}

// AbortWithError writes a normalized JSON error and aborts the chain.
func AbortWithError(c *gin.Context, status int, msg string) {
	c.AbortWithStatusJSON(status, ErrorResponse{
		Success: false,
		Error:   msg,
		Code:    status,
	})
}

// Recovery wraps gin.Recovery and returns JSON on panics.
func Recovery() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered any) {
		AbortWithError(c, http.StatusInternalServerError, "internal server error")
	})
}

// CORS returns a middleware allowing the given origins ("*" allows all).
// Intended for mobile clients (Expo web, React Native debug bundles).
func CORS(allowedOrigins []string) gin.HandlerFunc {
	if len(allowedOrigins) == 0 {
		allowedOrigins = []string{"*"}
	}
	allowAll := len(allowedOrigins) == 1 && allowedOrigins[0] == "*"
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[strings.TrimSpace(o)] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if allowAll {
			c.Header("Access-Control-Allow-Origin", "*")
		} else if origin != "" {
			if _, ok := allowed[origin]; ok {
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin")
			}
		}
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization, Accept")
		c.Header("Access-Control-Max-Age", "86400")
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
