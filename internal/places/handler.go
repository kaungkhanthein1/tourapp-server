package places

import (
	"errors"
	"net/http"
	"strconv"

	"travel-app-backend/internal/middleware"
	pkgmw "travel-app-backend/internal/pkg/middleware"
	"travel-app-backend/internal/pkg/pagination"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// Handler exposes the places domain over HTTP.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterPublic mounts discovery routes (no auth required).
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/places", h.ListPlaces)
	rg.GET("/places/trending", h.ListTrending)
	rg.GET("/places/categories", h.ListCategories)
	rg.GET("/places/:id", h.GetPlace)
	rg.GET("/foods", h.ListFoods)
}

// RegisterProtected mounts favorites routes (BearerAuth required upstream).
func (h *Handler) RegisterProtected(rg *gin.RouterGroup) {
	rg.GET("/favorites", h.ListFavorites)
	rg.POST("/favorites", h.AddFavorite)
	rg.DELETE("/favorites/:placeId", h.RemoveFavorite)
}

// ListPlaces handles GET /places.
//
//	@Summary		List places (Explore)
//	@Description	Supports filters: category, q (search), popular, trending; paginated via limit/offset.
//	@Tags			places
//	@Produce		json
//	@Param			category	query		string	false	"Category name"
//	@Param			q			query		string	false	"Search query"
//	@Param			popular		query		string	false	"true|false"
//	@Param			trending	query		string	false	"true|false"
//	@Param			limit		query		int		false	"Max items (1-100, default 20)"
//	@Param			offset		query		int		false	"Offset (default 0)"
//	@Success		200			{object}	map[string]any
//	@Router			/places [get]
func (h *Handler) ListPlaces(c *gin.Context) {
	f := PlaceFilter{
		Category: c.Query("category"),
		Query:    c.Query("q"),
		Page:     pagination.Parse(c),
	}
	if v := c.Query("popular"); v != "" {
		b := v == "true" || v == "1"
		f.IsPopular = &b
	}
	if v := c.Query("trending"); v != "" {
		b := v == "true" || v == "1"
		f.IsTrending = &b
	}

	list, total, err := h.service.ListPlaces(c.Request.Context(), f)
	if err != nil {
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to list places")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    emptyIfNil(list),
		"meta":    pagination.NewMeta(f.Page, total),
	})
}

// ListTrending handles GET /places/trending.
//
//	@Summary		Trending places ("Hot Right Now")
//	@Tags			places
//	@Produce		json
//	@Param			limit	query		int	false	"Max items"
//	@Success		200		{object}	map[string]any
//	@Router			/places/trending [get]
func (h *Handler) ListTrending(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	list, err := h.service.ListTrending(c.Request.Context(), limit)
	if err != nil {
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to list trending places")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": emptyIfNil(list)})
}

// ListCategories handles GET /places/categories.
//
//	@Summary		Explore category chips
//	@Tags			places
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Router			/places/categories [get]
func (h *Handler) ListCategories(c *gin.Context) {
	list, err := h.service.ListCategories(c.Request.Context())
	if err != nil {
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to list categories")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": emptyIfNil(list)})
}

// GetPlace handles GET /places/:id.
//
//	@Summary		Place detail with gallery
//	@Tags			places
//	@Produce		json
//	@Param			id	path		int	true	"Place ID"
//	@Success		200	{object}	map[string]any
//	@Failure		404	{object}	map[string]any
//	@Router			/places/{id} [get]
func (h *Handler) GetPlace(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid place id")
		return
	}
	p, err := h.service.GetPlace(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			middleware.AbortWithError(c, http.StatusNotFound, "place not found")
			return
		}
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to get place")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": p})
}

// ListFoods handles GET /foods.
//
//	@Summary		Food recommendations ("Foods You Should Try")
//	@Tags			places
//	@Produce		json
//	@Param			popular	query		string	false	"true for popular only"
//	@Success		200		{object}	map[string]any
//	@Router			/foods [get]
func (h *Handler) ListFoods(c *gin.Context) {
	popularOnly := c.Query("popular") == "true" || c.Query("popular") == "1"
	list, err := h.service.ListFoods(c.Request.Context(), popularOnly)
	if err != nil {
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to list foods")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": emptyIfNil(list)})
}

// ListFavorites handles GET /favorites (authenticated).
//
//	@Summary		List favorited places for the current user
//	@Tags			favorites
//	@Security		BearerAuth
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Failure		401	{object}	map[string]any
//	@Router			/favorites [get]
func (h *Handler) ListFavorites(c *gin.Context) {
	userID, ok := pkgmw.UserID(c)
	if !ok {
		middleware.AbortWithError(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	list, err := h.service.ListFavorites(c.Request.Context(), userID)
	if err != nil {
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to list favorites")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": emptyIfNil(list)})
}

// AddFavorite handles POST /favorites (authenticated).
//
//	@Summary		Favorite a place
//	@Tags			favorites
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		AddFavoriteRequest	true	"place_id required"
//	@Success		201		{object}	map[string]any
//	@Failure		401		{object}	map[string]any
//	@Router			/favorites [post]
func (h *Handler) AddFavorite(c *gin.Context) {
	userID, ok := pkgmw.UserID(c)
	if !ok {
		middleware.AbortWithError(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var req AddFavoriteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "place_id is required")
		return
	}
	if err := h.service.AddFavorite(c.Request.Context(), userID, req.PlaceID, req.Note); err != nil {
		if errors.Is(err, ErrNotFound) {
			middleware.AbortWithError(c, http.StatusNotFound, "place not found")
			return
		}
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to add favorite")
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "message": "favorited"})
}

// RemoveFavorite handles DELETE /favorites/:placeId (authenticated).
//
//	@Summary		Remove a favorite
//	@Tags			favorites
//	@Security		BearerAuth
//	@Produce		json
//	@Param			placeId	path		int	true	"Place ID"
//	@Success		200		{object}	map[string]any
//	@Failure		401		{object}	map[string]any
//	@Failure		404		{object}	map[string]any
//	@Router			/favorites/{placeId} [delete]
func (h *Handler) RemoveFavorite(c *gin.Context) {
	userID, ok := pkgmw.UserID(c)
	if !ok {
		middleware.AbortWithError(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	placeID, err := strconv.ParseInt(c.Param("placeId"), 10, 64)
	if err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid place id")
		return
	}
	if err := h.service.RemoveFavorite(c.Request.Context(), userID, placeID); err != nil {
		if errors.Is(err, ErrFavoriteNotFound) || errors.Is(err, pgx.ErrNoRows) {
			middleware.AbortWithError(c, http.StatusNotFound, "favorite not found")
			return
		}
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to remove favorite")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "unfavorited"})
}

func emptyIfNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
