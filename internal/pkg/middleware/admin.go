package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	// ContextRole is the resolved RBAC role set by AdminRequired.
	ContextRole = "auth.role"
	// RoleAdmin / RoleUser are the two values users.role can hold.
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// RoleStore resolves a user's role. Satisfied by *users.PostgresRepository —
// declared as a consumer-side interface so pkg/middleware never imports
// the users package (same pattern as FirebaseUserStore).
type RoleStore interface {
	GetRoleByFirebaseUID(ctx context.Context, uid string) (string, error)
}

// AdminRequired gates a route group to role == "admin". It runs after
// FirebaseAuthMiddleware (which sets ContextFirebaseUID, and the role
// claim when present).
//
// Authorization is always enforced server-side:
//   - Firebase custom claim "role=admin" is a fast path (no DB read);
//   - otherwise users.role in Neon is read and decides — so demoting a
//     admin takes effect on the very next request, while claim-based
//     access expires with the ID token (~1h) if the claim isn't cleared.
func AdminRequired(store RoleStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		uid, ok := FirebaseUID(c)
		if !ok {
			abortUnauthorized(c, "unauthenticated")
			return
		}

		role := ""
		if v, ok := c.Get(ContextRole); ok {
			role, _ = v.(string)
		}
		if role != RoleAdmin {
			dbRole, err := store.GetRoleByFirebaseUID(c.Request.Context(), uid)
			if err != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
					"success": false,
					"error":   "failed to resolve account role",
					"code":    http.StatusInternalServerError,
				})
				return
			}
			role = strings.TrimSpace(dbRole)
		}
		c.Set(ContextRole, role)

		if role != RoleAdmin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"success": false,
				"error":   "admin access required",
				"code":    http.StatusForbidden,
			})
			return
		}
		c.Next()
	}
}
