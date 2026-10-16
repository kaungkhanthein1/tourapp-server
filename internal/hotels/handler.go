package hotels

import (
	"errors"
	"net/http"
	"strconv"

	"travel-app-backend/internal/pkg/pagination"
	"travel-app-backend/internal/middleware"

	"github.com/gin-gonic/gin"
)

// Handler exposes the hotels domain over HTTP.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterPublic mounts public hotel routes (no auth required).
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/hotels", h.ListHotels)
	rg.GET("/hotels/:id", h.GetHotel)
	rg.GET("/hotels/:id/rooms", h.ListRooms)
}

// ListHotels handles GET /hotels.
//
//	@Summary		List hotels ( Hotels tab)
//	@Description	Supports filters: place, q; paginated via limit/offset.
//	@Tags			hotels
//	@Produce		json
//	@Param			place	query		string	false	"Place name"
//	@Param			q		query		string	false	"Search query"
//	@Param			limit	query		int		false	"Max items (1-100, default 20)"
//	@Param			offset	query		int		false	"Offset (default 0)"
//	@Success		200		{object}	map[string]any
//	@Router			/hotels [get]
func (h *Handler) ListHotels(c *gin.Context) {
	f := Filter{
		Place: c.Query("place"),
		Query: c.Query("q"),
		Page:  pagination.Parse(c),
	}
	list, total, err := h.service.ListHotels(c.Request.Context(), f)
	if err != nil {
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to list hotels")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    orEmpty(list),
		"meta":    pagination.NewMeta(f.Page, total),
	})
}

// GetHotel handles GET /hotels/:id.
//
//	@Summary		Hotel detail with facilities and rooms
//	@Tags			hotels
//	@Produce		json
//	@Param			id	path		int	true	"Hotel ID"
//	@Success		200	{object}	map[string]any
//	@Failure		404	{object}	map[string]any
//	@Router			/hotels/{id} [get]
func (h *Handler) GetHotel(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid hotel id")
		return
	}
	hotel, err := h.service.GetHotel(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			middleware.AbortWithError(c, http.StatusNotFound, "hotel not found")
			return
		}
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to get hotel")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": hotel})
}

// ListRooms handles GET /hotels/:id/rooms.
//
//	@Summary		Rooms with MMK pricing
//	@Tags			hotels
//	@Produce		json
//	@Param			id	path		int	true	"Hotel ID"
//	@Success		200	{object}	map[string]any
//	@Router			/hotels/{id}/rooms [get]
func (h *Handler) ListRooms(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid hotel id")
		return
	}
	rooms, err := h.service.ListRooms(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			middleware.AbortWithError(c, http.StatusNotFound, "hotel not found")
			return
		}
		middleware.AbortWithError(c, http.StatusInternalServerError, "failed to list rooms")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": orEmpty(rooms)})
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
