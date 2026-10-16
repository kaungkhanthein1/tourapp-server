package middleware

import (
	"context"
	"net/http"
	"strings"

	"travel-app-backend/internal/pkg/firebase"

	"github.com/gin-gonic/gin"
)

const (
	// ContextFirebaseUID is the verified Firebase UID set by FirebaseAuthMiddleware.
	ContextFirebaseUID = "auth.firebaseUID"
	// ContextEmailVerified reports whether the Firebase email is verified.
	ContextEmailVerified = "auth.emailVerified"
	// ContextUserName is the display name from the Firebase ID token.
	ContextUserName = "auth.userName"
)

// TokenVerifier verifies a Firebase ID token. Satisfied by *firebase.Client;
// declared as an interface so this package never imports the Firebase SDK.
type TokenVerifier interface {
	VerifyIDToken(ctx context.Context, idToken string) (*firebase.Identity, error)
}

// FirebaseUserStore upserts a Firebase identity into the users table.
// Satisfied by *users.PostgresRepository — declared here (consumer-side
// interface) so pkg/middleware never imports the users package (no cycle).
type FirebaseUserStore interface {
	GetOrCreateByFirebaseUID(ctx context.Context, uid, email, name string) (int64, error)
}

// FirebaseAuthMiddleware verifies Authorization: Bearer <firebase-id-token>,
// auto-syncs the user into Neon PostgreSQL, and injects the verified
// firebase_uid, email and local user_id into gin.Context. Blocks with 401
// when the token is missing/invalid, 500 when the user sync fails.
func FirebaseAuthMiddleware(verifier TokenVerifier, store FirebaseUserStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if header == "" {
			abortUnauthorized(c, "missing Authorization header")
			return
		}
		token, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || token == "" {
			abortUnauthorized(c, "Authorization header must be: Bearer <firebase-id-token>")
			return
		}

		identity, err := verifier.VerifyIDToken(c.Request.Context(), token)
		if err != nil {
			abortUnauthorized(c, "invalid or expired Firebase ID token")
			return
		}

		userID, err := store.GetOrCreateByFirebaseUID(
			c.Request.Context(), identity.UID, identity.Email, identity.Name,
		)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"success": false,
				"error":   "failed to sync user account",
				"code":    http.StatusInternalServerError,
			})
			return
		}

		c.Set(ContextUserID, userID)
		c.Set(ContextUserEmail, identity.Email)
		c.Set(ContextFirebaseUID, identity.UID)
		c.Set(ContextEmailVerified, identity.EmailVerified)
		c.Set(ContextUserName, identity.Name)
		c.Next()
	}
}

// FirebaseUID returns the verified Firebase UID from context.
func FirebaseUID(c *gin.Context) (string, bool) {
	v, ok := c.Get(ContextFirebaseUID)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// EmailVerified reports whether the token's email is verified.
func EmailVerified(c *gin.Context) bool {
	v, ok := c.Get(ContextEmailVerified)
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}
