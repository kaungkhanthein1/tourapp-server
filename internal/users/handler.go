package users

import (
	"errors"
	"net/http"
	"strconv"

	"travel-app-backend/internal/middleware"
	pkgmw "travel-app-backend/internal/pkg/middleware"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// Handler exposes the users domain over HTTP (Profile + auth screens).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterPublic mounts unauthenticated routes (login/register).
//
// Deprecated: password auth is superseded by Firebase Authentication on the
// client; these routes remain only for legacy/test clients.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.POST("/users/register", h.Register)
	rg.POST("/users/login", h.Login)
}

// RegisterProtected mounts authenticated routes (requires FirebaseAuthMiddleware upstream).
func (h *Handler) RegisterProtected(rg *gin.RouterGroup) {
	rg.POST("/users/sync", h.Sync)
	rg.GET("/users/me", h.Me)
	rg.PUT("/users/me", h.UpdateMe)
	rg.GET("/users/:id", h.GetByID)
	rg.PUT("/users/:id", h.Update)
}

// Sync handles POST /users/sync (authenticated).
// The Firebase middleware already upserted the account on this request;
// the endpoint lets the client explicitly re-trigger profile creation
// after email verification and read the resulting Neon row.
//
//	@Summary		Sync the verified Firebase identity into the users table
//	@Description	Idempotent: upserts by firebase_uid (claiming legacy password-era rows by email) and returns the local profile. Called by the mobile app after email verification.
//	@Tags			auth
//	@Security		BearerAuth
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Failure		401	{object}	map[string]any
//	@Failure		500	{object}	map[string]any
//	@Router			/users/sync [post]
func (h *Handler) Sync(c *gin.Context) {
	uid, ok := pkgmw.FirebaseUID(c)
	if !ok || uid == "" {
		middleware.AbortWithError(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	email := c.GetString(pkgmw.ContextUserEmail)
	name := c.GetString(pkgmw.ContextUserName)

	u, err := h.service.SyncFirebase(c.Request.Context(), uid, email, name)
	if err != nil {
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to sync user")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": u})
}

// Register handles POST /users/register.
//
// Deprecated: use Firebase Authentication on the client, then POST /users/sync.
//
//	@Summary		Register a new user
//	@Deprecated
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		RegisterRequest	true	"registration payload"
//	@Success		201		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Failure		409		{object}	map[string]any
//	@Router			/users/register [post]
func (h *Handler) Register(c *gin.Context) {
	var req RegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "name, valid email and password (min 8 chars) are required")
		return
	}
	res, err := h.service.Register(c.Request.Context(), req)
	if err != nil {
		if errors.Is(err, ErrEmailTaken) {
			middleware.AbortWithError(c, http.StatusConflict, "email already registered")
			return
		}
		middleware.AbortWithError(c, http.StatusInternalServerError, "registration failed")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": res})
}

// Login handles POST /users/login.
//
// Deprecated: use Firebase Authentication on the client; the backend only
// accepts Firebase ID tokens on protected routes.
//
//	@Summary		Login and receive a JWT
//	@Deprecated
//	@Tags			auth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		LoginRequest	true	"login payload"
//	@Success		200		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Failure		401		{object}	map[string]any
//	@Router			/users/login [post]
func (h *Handler) Login(c *gin.Context) {
	var req LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "email and password are required")
		return
	}
	res, err := h.service.Login(c.Request.Context(), req)
	if err != nil {
		if errors.Is(err, ErrBadCredentials) {
			middleware.AbortWithError(c, http.StatusUnauthorized, "invalid email or password")
			return
		}
		middleware.AbortWithError(c, http.StatusInternalServerError, "login failed")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": res})
}

// Me handles GET /users/me (authenticated).
func (h *Handler) Me(c *gin.Context) {
	id, ok := pkgmw.UserID(c)
	if !ok {
		middleware.AbortWithError(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	u, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to get user")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": u})
}

func (h *Handler) GetByID(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid user id")
		return
	}
	if caller, ok := pkgmw.UserID(c); !ok || caller != id {
		middleware.AbortWithError(c, http.StatusForbidden, "you may only access your own profile")
		return
	}
	u, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			middleware.AbortWithError(c, http.StatusNotFound, "user not found")
			return
		}
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to get user")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": u})
}

func (h *Handler) Update(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid user id")
		return
	}
	if caller, ok := pkgmw.UserID(c); !ok || caller != id {
		middleware.AbortWithError(c, http.StatusForbidden, "you may only update your own profile")
		return
	}
	h.applyUpdate(c, id)
}

// UpdateMe handles PUT /users/me (authenticated): updates the caller's own
// profile. Used by the mobile app to persist the Supabase Storage public URL
// in avatar_url after a client-side upload.
//
//	@Summary		Update the authenticated user's profile
//	@Description	Partially updates the caller's profile. avatar_url must be the public HTTPS URL of an image uploaded client-side to Supabase Storage; the server never receives file bytes.
//	@Tags			users
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		UpdateRequest	true	"Fields to update (all optional)"
//	@Success		200		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Failure		401		{object}	map[string]any
//	@Failure		404		{object}	map[string]any
//	@Router			/users/me [put]
func (h *Handler) UpdateMe(c *gin.Context) {
	id, ok := pkgmw.UserID(c)
	if !ok {
		middleware.AbortWithError(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	h.applyUpdate(c, id)
}

// applyUpdate binds the body and delegates to the service for a known user id.
func (h *Handler) applyUpdate(c *gin.Context, id int64) {
	var req UpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid JSON body")
		return
	}
	u, err := h.service.Update(c.Request.Context(), id, req)
	if err != nil {
		switch {
		case errors.Is(err, ErrInvalidAvatar):
			middleware.AbortWithError(c, http.StatusBadRequest, err.Error())
		case errors.Is(err, ErrNotFound), errors.Is(err, pgx.ErrNoRows):
			middleware.AbortWithError(c, http.StatusNotFound, "user not found")
		default:
			middleware.AbortWithError(c, http.StatusInternalServerError, "failed to update user")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": u})
}
