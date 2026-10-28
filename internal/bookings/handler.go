package bookings

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"travel-app-backend/internal/middleware"
	pkgmw "travel-app-backend/internal/pkg/middleware"

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

// RegisterPublic mounts the unauthenticated availability engine. It
// shares the /hotels/:id prefix with the hotels domain (same param name,
// so gin's tree accepts it) — availability is a booking concern: it
// reads bookings to compute free units.
func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/hotels/:id/availability", h.Availability)
}

// Availability handles GET /hotels/:id/availability (public).
//
//	@Summary		Real-time room availability for a date window
//	@Description	Per-room free units over [check_in, check_out): units_booked is the peak concurrent active holds on any single night; bookable=false when sold out or stop-sold (is_active=false).
//	@Tags			hotels
//	@Produce		json
//	@Param			id			path	int		true	"Hotel ID"
//	@Param			check_in	query	string	true	"YYYY-MM-DD (inclusive)"
//	@Param			check_out	query	string	true	"YYYY-MM-DD (exclusive)"
//	@Success		200			{object}	map[string]any
//	@Failure		400			{object}	map[string]any
//	@Failure		404			{object}	map[string]any
//	@Router			/hotels/{id}/availability [get]
func (h *Handler) Availability(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid hotel id")
		return
	}
	resp, err := h.service.Availability(c.Request.Context(), id, c.Query("check_in"), c.Query("check_out"))
	if err != nil {
		switch {
		case errors.Is(err, ErrHotelNotFound):
			middleware.AbortWithError(c, http.StatusNotFound, "hotel not found")
		case errors.Is(err, ErrInvalidDates):
			middleware.AbortWithError(c, http.StatusBadRequest, err.Error())
		default:
			middleware.AbortWithError(c, http.StatusInternalServerError, "internal server error")
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": resp})
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
//	@Summary		Book a hotel room — pay-at-property, born confirmed (409 when sold out)
//	@Description	Confirms instantly: payment is settled at the property, so the booking is created with status='confirmed'. Omit payment_method or pass 'pay_at_property'; any other value returns 400 until an online gateway exists.
//	@Tags			bookings
//	@Security		BearerAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body		CreateRequest	true	"hotel_id, room_id, check_in, check_out, payment_method (optional, default pay_at_property)"
//	@Success		201		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Failure		401		{object}	map[string]any
//	@Failure		409		{object}	map[string]any
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
	case errors.Is(err, ErrPaymentUnsupported):
		middleware.AbortWithError(c, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrNotPending):
		middleware.AbortWithError(c, http.StatusConflict, "only upcoming (pending or confirmed) bookings can be cancelled or rescheduled")
	case errors.Is(err, ErrNotAvailable):
		middleware.AbortWithError(c, http.StatusConflict, "room not available for the selected dates")
	case errors.Is(err, ErrRoomInactive):
		middleware.AbortWithError(c, http.StatusConflict, "room is not currently bookable")
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
