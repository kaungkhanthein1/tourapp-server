package middleware

import (
	"fmt"
	"time"

	"github.com/gin-gonic/gin"
)

// Logger logs method, path, status, and latency per request.
func Logger() gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(p gin.LogFormatterParams) string {
		line := fmt.Sprintf("[GIN] %s | %3d | %13v | %15s | %-7s %s\n",
			p.TimeStamp.Format(time.RFC3339),
			p.StatusCode,
			p.Latency,
			p.ClientIP,
			p.Method,
			p.Path,
		)
		if p.ErrorMessage != "" {
			line += fmt.Sprintf("  error: %s\n", p.ErrorMessage)
		}
		return line
	})
}
