package admin

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"travel-app-backend/internal/bookings"
	"travel-app-backend/internal/middleware"
	pkgmw "travel-app-backend/internal/pkg/middleware"
	"travel-app-backend/internal/pkg/pagination"

	"github.com/gin-gonic/gin"
)

// Handler exposes the admin extranet over HTTP. Every route it mounts
// sits behind AdminRequired (wired in cmd/server/main.go).
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// RegisterAdmin mounts the extranet routes on an already-guarded group.
func (h *Handler) RegisterAdmin(rg *gin.RouterGroup) {
	// Bookings
	rg.GET("/bookings", h.ListBookings)
	rg.GET("/bookings/:id", h.GetBooking)
	rg.PATCH("/bookings/:id/status", h.ChangeBookingStatus)
	rg.PATCH("/bookings/:id", h.EditBooking)

	// Hotels & rooms
	rg.GET("/hotels", h.ListHotels)
	rg.POST("/hotels", h.CreateHotel)
	rg.GET("/hotels/:id", h.GetHotel)
	rg.PATCH("/hotels/:id", h.UpdateHotel)
	rg.DELETE("/hotels/:id", h.DeleteHotel)
	rg.POST("/hotels/:id/rooms", h.CreateRoom)
	rg.PATCH("/hotels/:id/rooms/:rid", h.UpdateRoom)
	rg.DELETE("/hotels/:id/rooms/:rid", h.DeleteRoom)
	rg.PUT("/hotels/:id/rooms/:rid/inventory", h.SetRoomInventory)

	// Stats
	rg.GET("/stats/overview", h.StatsOverview)
	rg.GET("/stats/occupancy", h.Occupancy)

	// Users
	rg.GET("/users", h.ListUsers)
	rg.PATCH("/users/:id/role", h.SetUserRole)

	// Audit
	rg.GET("/audit-logs", h.ListAuditLogs)
}

// adminID reads the authenticated admin's user id (set by the Firebase
// auth middleware) for audit attribution. 0 when absent.
func adminID(c *gin.Context) int64 {
	id, _ := c.Get(pkgmw.ContextUserID)
	switch v := id.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// writeServiceError maps domain errors to HTTP responses.
func writeServiceError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		middleware.AbortWithError(c, http.StatusNotFound, "not found")
	case errors.Is(err, bookings.ErrNotAvailable):
		middleware.AbortWithError(c, http.StatusConflict, "room not available for the selected dates")
	case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrHotelHasBookings), errors.Is(err, ErrRoomHasBookings):
		middleware.AbortWithError(c, http.StatusConflict, err.Error())
	case errors.Is(err, ErrInvalidDates), errors.Is(err, ErrInvalidStatus), errors.Is(err, ErrInvalidRole):
		middleware.AbortWithError(c, http.StatusBadRequest, err.Error())
	default:
		middleware.AbortWithError(c, http.StatusInternalServerError, "internal server error")
	}
}

// idParam parses a :id path parameter.
func idParam(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid "+name)
		return 0, false
	}
	return id, true
}

// ── Bookings ──────────────────────────────────────────────────────────

// ListBookings handles GET /admin/bookings.
//
//	@Summary		List all bookings (extranet)
//	@Tags			admin-bookings
//	@Security		AdminAuth
//	@Produce		json
//	@Param			status	query		string	false	"pending | confirmed | complete | cancelled"
//	@Param			hotel_id	query		int		false	"Filter by hotel"
//	@Param			q		query		string	false	"Guest name/phone/email or hotel name"
//	@Param			from	query		string	false	"check_in >= from (YYYY-MM-DD)"
//	@Param			to		query		string	false	"check_in < to (YYYY-MM-DD)"
//	@Param			limit	query		int		false	"Max items (1-100, default 20)"
//	@Param			offset	query		int		false	"Offset"
//	@Success		200		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Failure		401		{object}	map[string]any
//	@Failure		403		{object}	map[string]any
//	@Router			/admin/bookings [get]
func (h *Handler) ListBookings(c *gin.Context) {
	page := pagination.Parse(c)
	hotelID, _ := strconv.ParseInt(c.Query("hotel_id"), 10, 64)

	items, total, err := h.service.ListBookings(c.Request.Context(), BookingFilter{
		Status:  c.Query("status"),
		HotelID: hotelID,
		Q:       c.Query("q"),
		From:    c.Query("from"),
		To:      c.Query("to"),
		Page:    page,
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "meta": pagination.NewMeta(page, total)})
}

// GetBooking handles GET /admin/bookings/:id.
//
//	@Summary		Get one booking
//	@Tags			admin-bookings
//	@Security		AdminAuth
//	@Produce		json
//	@Param			id	path		int	true	"Booking ID"
//	@Success		200	{object}	map[string]any
//	@Failure		401	{object}	map[string]any
//	@Failure		404	{object}	map[string]any
//	@Router			/admin/bookings/{id} [get]
func (h *Handler) GetBooking(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	b, err := h.service.GetBooking(c.Request.Context(), id)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": b})
}

// ChangeBookingStatus handles PATCH /admin/bookings/:id/status.
//
//	@Summary		Transition a booking status (pending→confirmed/cancelled, confirmed→complete/cancelled)
//	@Tags			admin-bookings
//	@Security		AdminAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		int					true	"Booking ID"
//	@Param			body	body		UpdateStatusRequest	true	"status + optional reason (audited)"
//	@Success		200		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Failure		409		{object}	map[string]any
//	@Router			/admin/bookings/{id}/status [patch]
func (h *Handler) ChangeBookingStatus(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var req UpdateStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid body: status required")
		return
	}
	b, err := h.service.ChangeStatus(c.Request.Context(), adminID(c), id, req)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": b})
}

// EditBooking handles PATCH /admin/bookings/:id.
//
//	@Summary		Edit guest fields/dates of a pending or confirmed booking
//	@Tags			admin-bookings
//	@Security		AdminAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path		int					true	"Booking ID"
//	@Param			body	body		UpdateBookingRequest	true	"Partial update"
//	@Success		200		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Failure		409		{object}	map[string]any
//	@Router			/admin/bookings/{id} [patch]
func (h *Handler) EditBooking(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var req UpdateBookingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid body")
		return
	}
	b, err := h.service.EditBooking(c.Request.Context(), adminID(c), id, req)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": b})
}

// ── Hotels & rooms ────────────────────────────────────────────────────

// ListHotels handles GET /admin/hotels.
//
//	@Summary		List hotels with room/active-booking counts
//	@Tags			admin-hotels
//	@Security		AdminAuth
//	@Produce		json
//	@Param			q		query	string	false	"Name/place search"
//	@Param			limit	query	int		false	"Max items (1-100)"
//	@Param			offset	query	int		false	"Offset"
//	@Success		200		{object}	map[string]any
//	@Router			/admin/hotels [get]
func (h *Handler) ListHotels(c *gin.Context) {
	page := pagination.Parse(c)
	items, total, err := h.service.ListHotels(c.Request.Context(), c.Query("q"), page)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "meta": pagination.NewMeta(page, total)})
}

// CreateHotel handles POST /admin/hotels.
//
//	@Summary		Create a hotel
//	@Tags			admin-hotels
//	@Security		AdminAuth
//	@Accept			json
//	@Produce		json
//	@Param			body	body	CreateHotelRequest	true	"Hotel"
//	@Success		201		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Router			/admin/hotels [post]
func (h *Handler) CreateHotel(c *gin.Context) {
	var req CreateHotelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid body: name and place are required")
		return
	}
	hotel, err := h.service.CreateHotel(c.Request.Context(), adminID(c), req)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": hotel})
}

// GetHotel handles GET /admin/hotels/:id.
//
//	@Summary		Get a hotel with its rooms
//	@Tags			admin-hotels
//	@Security		AdminAuth
//	@Produce		json
//	@Param			id	path	int	true	"Hotel ID"
//	@Success		200	{object}	map[string]any
//	@Failure		404	{object}	map[string]any
//	@Router			/admin/hotels/{id} [get]
func (h *Handler) GetHotel(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	hotel, err := h.service.GetHotel(c.Request.Context(), id)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": hotel})
}

// UpdateHotel handles PATCH /admin/hotels/:id.
//
//	@Summary		Patch a hotel (supplied fields only)
//	@Tags			admin-hotels
//	@Security		AdminAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path	int					true	"Hotel ID"
//	@Param			body	body	UpdateHotelRequest	true	"Partial update"
//	@Success		200		{object}	map[string]any
//	@Failure		404		{object}	map[string]any
//	@Router			/admin/hotels/{id} [patch]
func (h *Handler) UpdateHotel(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var req UpdateHotelRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid body")
		return
	}
	hotel, err := h.service.UpdateHotel(c.Request.Context(), adminID(c), id, req)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": hotel})
}

// DeleteHotel handles DELETE /admin/hotels/:id.
//
//	@Summary		Delete a hotel (409 while any booking references it)
//	@Tags			admin-hotels
//	@Security		AdminAuth
//	@Produce		json
//	@Param			id	path	int	true	"Hotel ID"
//	@Success		204	{object}	map[string]any
//	@Failure		404	{object}	map[string]any
//	@Failure		409	{object}	map[string]any
//	@Router			/admin/hotels/{id} [delete]
func (h *Handler) DeleteHotel(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	if err := h.service.DeleteHotel(c.Request.Context(), adminID(c), id); err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusNoContent, gin.H{"success": true})
}

// CreateRoom handles POST /admin/hotels/:id/rooms.
//
//	@Summary		Add a room type to a hotel
//	@Tags			admin-hotels
//	@Security		AdminAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path	int			true	"Hotel ID"
//	@Param			body	body	RoomRequest	true	"Room"
//	@Success		201		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Failure		404		{object}	map[string]any
//	@Router			/admin/hotels/{id}/rooms [post]
func (h *Handler) CreateRoom(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var req RoomRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid body: name and price_mmk are required")
		return
	}
	room, err := h.service.CreateRoom(c.Request.Context(), adminID(c), id, req)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusCreated, gin.H{"success": true, "data": room})
}

// UpdateRoom handles PATCH /admin/hotels/:id/rooms/:rid.
//
//	@Summary		Patch a room
//	@Tags			admin-hotels
//	@Security		AdminAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path	int			true	"Hotel ID"
//	@Param			rid		path	int			true	"Room ID"
//	@Param			body	body	RoomRequest	true	"Room"
//	@Success		200		{object}	map[string]any
//	@Failure		404		{object}	map[string]any
//	@Router			/admin/hotels/{id}/rooms/{rid} [patch]
func (h *Handler) UpdateRoom(c *gin.Context) {
	hotelID, ok := idParam(c, "id")
	if !ok {
		return
	}
	roomID, ok := idParam(c, "rid")
	if !ok {
		return
	}
	var req RoomRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid body: name and price_mmk are required")
		return
	}
	room, err := h.service.UpdateRoom(c.Request.Context(), adminID(c), hotelID, roomID, req)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": room})
}

// DeleteRoom handles DELETE /admin/hotels/:id/rooms/:rid.
//
//	@Summary		Delete a room (409 when bookings reference it — deactivate instead)
//	@Tags			admin-hotels
//	@Security		AdminAuth
//	@Produce		json
//	@Param			id	path	int	true	"Hotel ID"
//	@Param			rid	path	int	true	"Room ID"
//	@Success		204	{object}	map[string]any
//	@Failure		404	{object}	map[string]any
//	@Failure		409	{object}	map[string]any
//	@Router			/admin/hotels/{id}/rooms/{rid} [delete]
func (h *Handler) DeleteRoom(c *gin.Context) {
	hotelID, ok := idParam(c, "id")
	if !ok {
		return
	}
	roomID, ok := idParam(c, "rid")
	if !ok {
		return
	}
	if err := h.service.DeleteRoom(c.Request.Context(), adminID(c), hotelID, roomID); err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusNoContent, gin.H{"success": true})
}

// SetRoomInventory handles PUT /admin/hotels/:id/rooms/:rid/inventory.
//
//	@Summary		Set room allotment (total_units) and stop-sell (is_active)
//	@Tags			admin-hotels
//	@Security		AdminAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path	int					true	"Hotel ID"
//	@Param			rid		path	int					true	"Room ID"
//	@Param			body	body	InventoryRequest	true	"Inventory"
//	@Success		200		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Failure		404		{object}	map[string]any
//	@Router			/admin/hotels/{id}/rooms/{rid}/inventory [put]
func (h *Handler) SetRoomInventory(c *gin.Context) {
	hotelID, ok := idParam(c, "id")
	if !ok {
		return
	}
	roomID, ok := idParam(c, "rid")
	if !ok {
		return
	}
	var req InventoryRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid body: total_units >= 1 required")
		return
	}
	room, err := h.service.SetRoomInventory(c.Request.Context(), adminID(c), hotelID, roomID, req)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": room})
}

// ── Stats ─────────────────────────────────────────────────────────────

// StatsOverview handles GET /admin/stats/overview.
//
//	@Summary		Dashboard counters (bookings by status, revenue, catalog, users)
//	@Tags			admin-stats
//	@Security		AdminAuth
//	@Produce		json
//	@Success		200	{object}	map[string]any
//	@Router			/admin/stats/overview [get]
func (h *Handler) StatsOverview(c *gin.Context) {
	stats, err := h.service.StatsOverview(c.Request.Context())
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": stats})
}

// Occupancy handles GET /admin/stats/occupancy.
//
//	@Summary		Per-night occupancy for [from, to) — same checkout-exclusive semantics as the booking constraint
//	@Tags			admin-stats
//	@Security		AdminAuth
//	@Produce		json
//	@Param			from	query	string	true	"YYYY-MM-DD inclusive"
//	@Param			to		query	string	true	"YYYY-MM-DD exclusive"
//	@Param			hotel_id	query	int		false	"Limit to one hotel"
//	@Success		200		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Router			/admin/stats/occupancy [get]
func (h *Handler) Occupancy(c *gin.Context) {
	hotelID, _ := strconv.ParseInt(c.Query("hotel_id"), 10, 64)
	days, err := h.service.Occupancy(c.Request.Context(), c.Query("from"), c.Query("to"), hotelID)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": days})
}

// ── Users ─────────────────────────────────────────────────────────────

// ListUsers handles GET /admin/users.
//
//	@Summary		List users with booking counts
//	@Tags			admin-users
//	@Security		AdminAuth
//	@Produce		json
//	@Param			q		query	string	false	"Name/email search"
//	@Param			role	query	string	false	"user | admin"
//	@Param			limit	query	int		false	"Max items (1-100)"
//	@Param			offset	query	int		false	"Offset"
//	@Success		200		{object}	map[string]any
//	@Router			/admin/users [get]
func (h *Handler) ListUsers(c *gin.Context) {
	page := pagination.Parse(c)
	items, total, err := h.service.ListUsers(c.Request.Context(), c.Query("q"), c.Query("role"), page)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "meta": pagination.NewMeta(page, total)})
}

// SetUserRole handles PATCH /admin/users/:id/role.
//
//	@Summary		Promote/demote a user (DB role is the revocable source of truth)
//	@Tags			admin-users
//	@Security		AdminAuth
//	@Accept			json
//	@Produce		json
//	@Param			id		path	int				true	"User ID"
//	@Param			body	body	SetRoleRequest	true	"role"
//	@Success		200		{object}	map[string]any
//	@Failure		400		{object}	map[string]any
//	@Failure		404		{object}	map[string]any
//	@Router			/admin/users/{id} [patch]
func (h *Handler) SetUserRole(c *gin.Context) {
	id, ok := idParam(c, "id")
	if !ok {
		return
	}
	var req SetRoleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.AbortWithError(c, http.StatusBadRequest, "invalid body: role required")
		return
	}
	user, err := h.service.SetUserRole(c.Request.Context(), adminID(c), id, req)
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": user})
}

// ── Audit ─────────────────────────────────────────────────────────────

// ListAuditLogs handles GET /admin/audit-logs.
//
//	@Summary		Admin action trail (every extranet mutation)
//	@Tags			admin-audit
//	@Security		AdminAuth
//	@Produce		json
//	@Param			entity_type	query	string	false	"booking | hotel | room | user"
//	@Param			entity_id	query	int		false	"Filter by entity"
//	@Param			limit		query	int		false	"Max items (1-100)"
//	@Param			offset		query	int		false	"Offset"
//	@Success		200			{object}	map[string]any
//	@Router			/admin/audit-logs [get]
func (h *Handler) ListAuditLogs(c *gin.Context) {
	page := pagination.Parse(c)
	entityID, _ := strconv.ParseInt(c.Query("entity_id"), 10, 64)
	items, total, err := h.service.ListAuditLogs(c.Request.Context(), AuditFilter{
		EntityType: strings.TrimSpace(c.Query("entity_type")),
		EntityID:   entityID,
		Page:       page,
	})
	if err != nil {
		writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": items, "meta": pagination.NewMeta(page, total)})
}
