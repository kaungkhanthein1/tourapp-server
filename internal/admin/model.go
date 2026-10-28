package admin

import (
	"time"

	"travel-app-backend/internal/pkg/pagination"
)

// ── Bookings ──────────────────────────────────────────────────────────

// AdminBooking is the extranet view of a booking: everything the mobile
// Booking shows plus identity, pricing and lifecycle timestamps.
type AdminBooking struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	UserEmail string `json:"user_email,omitempty"`
	UserName  string `json:"user_name,omitempty"`
	HotelID   int64  `json:"hotel_id"`
	RoomID    int64  `json:"room_id"`
	UnitNo    int    `json:"unit_no"`
	// How the stay is paid: 'pay_at_property' (default) until a gateway lands.
	PaymentMethod string     `json:"payment_method"`
	HotelName     string     `json:"hotel_name"`
	RoomName      string     `json:"room_name"`
	Place         string     `json:"place"`
	ImageURL      string     `json:"image_url"`
	PriceMMK      int64      `json:"price_mmk"`
	Nights        int        `json:"nights"`
	TotalMMK      int64      `json:"total_mmk"`
	GuestName     string     `json:"guest_name,omitempty"`
	GuestPhone    string     `json:"guest_phone,omitempty"`
	CheckIn       string     `json:"check_in"`
	CheckOut      string     `json:"check_out"`
	Guests        int        `json:"guests"`
	Status        string     `json:"status"`
	ConfirmedAt   *time.Time `json:"confirmed_at,omitempty"`
	CancelledAt   *time.Time `json:"cancelled_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// BookingFilter drives GET /admin/bookings.
type BookingFilter struct {
	Status  string // '', pending, confirmed, complete, cancelled
	HotelID int64  // 0 = all
	Q       string // guest name/phone/email or hotel name substring
	From    string // YYYY-MM-DD inclusive lower bound on check_in ("" = none)
	To      string // YYYY-MM-DD exclusive upper bound on check_in ("" = none)
	Page    pagination.Page
}

// UpdateStatusRequest is the PATCH /admin/bookings/:id/status body.
// Reason is stored in the audit trail only (bookings has no reason column).
type UpdateStatusRequest struct {
	Status string `json:"status" binding:"required"`
	Reason string `json:"reason"`
}

// UpdateBookingRequest is the PATCH /admin/bookings/:id body — pointer
// fields so only the supplied columns are touched.
type UpdateBookingRequest struct {
	GuestName  *string `json:"guest_name"`
	GuestPhone *string `json:"guest_phone"`
	CheckIn    *string `json:"check_in"`
	CheckOut   *string `json:"check_out"`
	Guests     *int    `json:"guests"`
}

// ── Hotels & rooms ────────────────────────────────────────────────────

// AdminHotel is the extranet hotel view with room + booking counts.
type AdminHotel struct {
	ID             int64       `json:"id"`
	Name           string      `json:"name"`
	Place          string      `json:"place"`
	Country        string      `json:"country"`
	Region         string      `json:"region,omitempty"`
	ImageURL       string      `json:"image_url"`
	GalleryURLs    []string    `json:"gallery_urls"`
	Rating         float64     `json:"rating"`
	Facilities     []string    `json:"facilities"`
	PlaceID        *int64      `json:"place_id,omitempty"`
	Source         string      `json:"source,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	RoomCount      int64       `json:"room_count"`
	ActiveBookings int64       `json:"active_bookings"` // pending + confirmed
	Rooms          []AdminRoom `json:"rooms,omitempty"`
}

// AdminRoom adds the Phase 0 inventory columns the public Room hides.
type AdminRoom struct {
	ID             int64  `json:"id"`
	HotelID        int64  `json:"hotel_id"`
	Name           string `json:"name"`
	PriceMMK       int64  `json:"price_mmk"`
	Capacity       int    `json:"capacity"`
	ImageURL       string `json:"image_url"`
	Description    string `json:"description,omitempty"`
	TotalUnits     int    `json:"total_units"` // allotment
	IsActive       bool   `json:"is_active"`   // stop-sell switch
	ActiveBookings int64  `json:"active_bookings"`
}

// CreateHotelRequest is the POST /admin/hotels body.
type CreateHotelRequest struct {
	Name        string   `json:"name" binding:"required"`
	Place       string   `json:"place" binding:"required"`
	Country     string   `json:"country"`
	Region      string   `json:"region"`
	ImageURL    string   `json:"image_url"`
	GalleryURLs []string `json:"gallery_urls"`
	Rating      float64  `json:"rating"`
	Facilities  []string `json:"facilities"`
	PlaceID     *int64   `json:"place_id"`
}

// UpdateHotelRequest is the PATCH /admin/hotels/:id body — pointers so
// only supplied fields are written.
type UpdateHotelRequest struct {
	Name        *string   `json:"name"`
	Place       *string   `json:"place"`
	Country     *string   `json:"country"`
	Region      *string   `json:"region"`
	ImageURL    *string   `json:"image_url"`
	GalleryURLs *[]string `json:"gallery_urls"`
	Rating      *float64  `json:"rating"`
	Facilities  *[]string `json:"facilities"`
}

// RoomRequest is the POST/PATCH room body (inventory set separately).
type RoomRequest struct {
	Name        string `json:"name" binding:"required"`
	PriceMMK    int64  `json:"price_mmk" binding:"required,min=0"`
	Capacity    int    `json:"capacity" binding:"min=1"`
	ImageURL    string `json:"image_url"`
	Description string `json:"description"`
	TotalUnits  int    `json:"total_units" binding:"min=1"`
	IsActive    *bool  `json:"is_active"` // nil → true on create
}

// InventoryRequest is the PUT /admin/hotels/:id/rooms/:rid/inventory body.
type InventoryRequest struct {
	TotalUnits int   `json:"total_units" binding:"required,min=1"`
	IsActive   *bool `json:"is_active"`
}

// ── Users ─────────────────────────────────────────────────────────────

// AdminUser is the extranet user row.
type AdminUser struct {
	ID            int64     `json:"id"`
	FirebaseUID   string    `json:"firebase_uid,omitempty"`
	Name          string    `json:"name"`
	Email         string    `json:"email"`
	Role          string    `json:"role"`
	Phone         string    `json:"phone,omitempty"`
	TotalBookings int64     `json:"total_bookings"`
	CreatedAt     time.Time `json:"created_at"`
}

// SetRoleRequest is the PATCH /admin/users/:id/role body.
type SetRoleRequest struct {
	Role string `json:"role" binding:"required"`
}

// ── Stats ─────────────────────────────────────────────────────────────

// OverviewStats is GET /admin/stats/overview.
type OverviewStats struct {
	TotalBookings     int64 `json:"total_bookings"`
	PendingBookings   int64 `json:"pending_bookings"`
	ConfirmedBookings int64 `json:"confirmed_bookings"`
	CompleteBookings  int64 `json:"complete_bookings"`
	CancelledBookings int64 `json:"cancelled_bookings"`
	ActiveHolds       int64 `json:"active_holds"`      // pending + confirmed (rooms currently held)
	RevenueMMK        int64 `json:"revenue_mmk"`       // completed stays
	PendingValueMMK   int64 `json:"pending_value_mmk"` // confirmed + pending pipeline
	Hotels            int64 `json:"hotels"`
	Rooms             int64 `json:"rooms"`
	UnitsForSale      int64 `json:"units_for_sale"` // sum of active rooms' total_units
	Users             int64 `json:"users"`
}

// OccupancyDay is one row of GET /admin/stats/occupancy.
type OccupancyDay struct {
	Date         string  `json:"date"`
	BookedNights int64   `json:"booked_nights"` // active bookings covering this night
	UnitsForSale int64   `json:"units_for_sale"`
	OccupancyPct float64 `json:"occupancy_pct"` // booked_nights / units_for_sale
}

// ── Audit ─────────────────────────────────────────────────────────────

// AuditLog records every admin mutation (who, what, when, payload).
type AuditLog struct {
	ID         int64          `json:"id"`
	AdminID    *int64         `json:"admin_id,omitempty"`
	AdminEmail string         `json:"admin_email,omitempty"`
	Action     string         `json:"action"`      // e.g. booking.status_changed
	EntityType string         `json:"entity_type"` // booking | hotel | room | user
	EntityID   int64          `json:"entity_id"`
	Details    map[string]any `json:"details,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// AuditFilter drives GET /admin/audit-logs.
type AuditFilter struct {
	EntityType string
	EntityID   int64 // 0 = all
	Page       pagination.Page
}
