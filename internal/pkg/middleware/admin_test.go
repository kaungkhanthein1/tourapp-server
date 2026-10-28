package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// fakeRoleStore is the RoleStore double; called counts DB lookups so the
// claim fast path can be proven to skip the database entirely.
type fakeRoleStore struct {
	role   string
	err    error
	called int
}

func (f *fakeRoleStore) GetRoleByFirebaseUID(context.Context, string) (string, error) {
	f.called++
	return f.role, f.err
}

// adminRequest runs a request through the middleware chain a route group
// would use: context seeder (what FirebaseAuthMiddleware would set) →
// AdminRequired → handler.
func adminRequest(store RoleStore, uid, claimRole string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/admin/ping",
		func(c *gin.Context) {
			if uid != "" {
				c.Set(ContextFirebaseUID, uid)
			}
			if claimRole != "" {
				c.Set(ContextRole, claimRole)
			}
		},
		AdminRequired(store),
		func(c *gin.Context) {
			role, _ := c.Get(ContextRole)
			c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"role": role}})
		},
	)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/ping", nil))
	return w
}

func TestAdminRequired(t *testing.T) {
	t.Run("missing auth context is 401", func(t *testing.T) {
		store := &fakeRoleStore{role: RoleAdmin}
		w := adminRequest(store, "", "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", w.Code)
		}
		if store.called != 0 {
			t.Fatalf("unauthenticated request must not hit the role store")
		}
	})

	t.Run("regular user is 403", func(t *testing.T) {
		store := &fakeRoleStore{role: RoleUser}
		w := adminRequest(store, "uid-1", "")
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", w.Code)
		}
		if !strings.Contains(w.Body.String(), "admin access required") {
			t.Fatalf("body must say admin access required, got: %s", w.Body.String())
		}
		if store.called != 1 {
			t.Fatalf("role must be resolved from the store exactly once, got %d", store.called)
		}
	})

	t.Run("admin via DB role is 200", func(t *testing.T) {
		store := &fakeRoleStore{role: RoleAdmin}
		w := adminRequest(store, "uid-2", "")
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"role":"admin"`) {
			t.Fatalf("response must expose role=admin, got: %s", w.Body.String())
		}
	})

	t.Run("admin claim fast path skips the database", func(t *testing.T) {
		// err would fail the request if the store were consulted.
		store := &fakeRoleStore{err: errors.New("store must not be called")}
		w := adminRequest(store, "uid-3", RoleAdmin)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 via claim fast path, got %d", w.Code)
		}
		if store.called != 0 {
			t.Fatalf("claim fast path must not hit the role store, got %d calls", store.called)
		}
	})

	t.Run("claim of non-admin still consults the DB", func(t *testing.T) {
		// A stale 'user' claim must not shadow a DB promotion.
		store := &fakeRoleStore{role: RoleAdmin}
		w := adminRequest(store, "uid-4", RoleUser)
		if w.Code != http.StatusOK {
			t.Fatalf("DB admin must win over a stale user claim, got %d", w.Code)
		}
		if store.called != 1 {
			t.Fatalf("stale user claim must fall through to the store, got %d calls", store.called)
		}
	})

	t.Run("store failure is 500", func(t *testing.T) {
		store := &fakeRoleStore{err: errors.New("db down")}
		w := adminRequest(store, "uid-5", "")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d", w.Code)
		}
	})
}
