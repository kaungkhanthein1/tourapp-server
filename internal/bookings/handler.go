package bookings

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	pkgmw "travel-app-backend/internal/pkg/middleware"
	"travel-app-backend/internal/middleware"

	"github.com/gin-gonic/gin"
)

// Handler exposes the bookings domain over HTTP (Booking History screen).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler {
	return &Handler{service: service}
}

// RegisterProtected mounts booking routes (BearerAuth required upstream).
func (h *Handler) RegisterProtected(rg *gin.RouterGroup) {
	rg.GET("/bookings", h.List)
	rg.GET("/bookings/:id", h.GetByID)
	rg.POST("/bookings", h.Create)
	rg.PATCH("/bookings/:id/cancel", h.Cancel)
	rg.PATCH("/bookings/:id/reschedule", h.Reschedule)
}

// List handles GET /bookings (authenticated).
//
//	@Summary		Booking history for the current user (Pending/Complete tabs)
//	@Tags			bookings
//	@Security		BearerAuth
//	@Produce		json
//	@Param			status	query		string	false	"pending | complete | cancelled"
//	@Success		200		{object}	map[string]any
//	@Failure		401		{object}	map[string]any
//	@Router			/bookings [get]
func (h *Handler) List(c *gin.Context) {
	userID, ok := pkgmw.UserID(c)
	if !ok {
		middleware.AbortWithError(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	status := c.Query("status") // "", "pending", "complete"
	list, err := h.service.ListByUser(c.Request.Context(), userID, status)
	if err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, err.Error())
		return
	}
	if list == nil {
		list = []Booking{}
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": list})
}

func (h *Handler) GetByID(c *gin.Context) {
	userID, ok := pkgmw.UserID(c)
	if !ok {
		middleware.AbortWithError(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid booking id")
		return
	}
	b, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	if b.UserID != userID {
		middleware.AbortWithError(c, http.StatusForbidden, "you do not own this booking")
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": b})
}

// Create handles POST /bookings (authenticated).
//
//	@Summary		Book a hotel room
//	@Tags			bookings
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		CreateRequest	true	"hotel_id, room_id, check_in, check_out"
//	@Success		201		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Failure		401		{object}	map[string]any
//	@Router			/bookings [post]
func (h *Handler) Create(c *gin.Context) {
	userID, ok := pkgmw.UserID(c)
	if !ok {
		middleware.AbortWithError(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	var req CreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "hotel_id, room_id, check_in, check_out are required")
		return
	}
	b, err := h.service.Create(c.Request.Context(), userID, req)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": b})
}

// Cancel handles PATCH /bookings/:id/cancel (authenticated, owner, pending only).
//
//	@Summary		Cancel a pending booking
//	@Tags			bookings
//	@Security		BearerAuth
//	@Produce		json
//	@Param			id	path		int	true	"Booking ID"
//	@Success		200		{object}	map[string]any
//	@Failure		403		{object}	map[string]any
//	@Failure		409		{object}	map[string]any
//	@Router			/bookings/{id}/cancel [patch]
func (h *Handler) Cancel(c *gin.Context) {
	userID, ok := pkgmw.UserID(c)
	if !ok {
		middleware.AbortWithError(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid booking id")
		return
	}
	b, err := h.service.Cancel(c.Request.Context(), userID, id)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": b})
}

// Reschedule handles PATCH /bookings/:id/reschedule (authenticated, owner, pending only).
//
//	@Summary		Reschedule a pending booking
//	@Tags			bookings
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		int					true	"Booking ID"
//	@Param			body	body		RescheduleRequest	true	"new check_in / check_out"
//	@Success		200		{object}	map[string]any
//	@Failure		403		{object}	map[string]any
//	@Router			/bookings/{id}/reschedule [patch]
func (h *Handler) Reschedule(c *gin.Context) {
	userID, ok := pkgmw.UserID(c)
	if !ok {
		middleware.AbortWithError(c, http.StatusUnauthorized, "unauthenticated")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid booking id")
		return
	}
	var req RescheduleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "check_in and check_out are required (YYYY-MM-DD)")
		return
	}
	b, err := h.service.Reschedule(c.Request.Context(), userID, id, req.CheckIn, req.CheckOut)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": b})
}

func writeServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		middleware.AbortWithError(c, http.StatusNotFound, "booking not found")
	case errors.Is(err, ErrRoomNotFound):
		middleware.AbortWithError(c, http.StatusNotFound, "room not found for hotel")
	case errors.Is(err, ErrInvalidDates):
		middleware.AbortWithError(c, http.StatusBadRequest, "check_out must be after check_in")
	case errors.Is(err, ErrNotPending):
		middleware.AbortWithError(c, http.StatusConflict, "only pending bookings can be cancelled or rescheduled")
	case errors.Is(err, ErrForbidden):
		middleware.AbortWithError(c, http.StatusForbidden, "you do not own this booking")
	default:
		msg := err.Error()
		switch {
		case strings.Contains(msg, "invalid check_in"):
			middleware.AbortWithError(c, http.StatusBadRequest, "invalid check_in, use YYYY-MM-DD")
		case strings.Contains(msg, "invalid check_out"):
			middleware.AbortWithError(c, http.StatusBadRequest, "invalid check_out, use YYYY-MM-DD")
		case strings.Contains(msg, "invalid status"):
			middleware.AbortWithError(c, http.StatusBadRequest, msg)
		default:
			middleware.AbortWithError(c, http.StatusInternalServerError, "internal error")
		}
	}
}
