package bookings

import "time"

// Status values for a booking (Pending/Complete tabs in Figma).
// 'confirmed' was added by Phase 0 for the full machine:
// pending → confirmed → complete, with cancelled reachable from pending
// or confirmed (owner cancel, or admin). Under pay-at-property (Phase 3)
// a successful booking is BORN confirmed — no online money moves, so
// there is no tentative-hold stage.
const (
	StatusPending   = "pending"
	StatusConfirmed = "confirmed"
	StatusComplete  = "complete"
	StatusCancelled = "cancelled"
)

// Payment methods. Pay-at-property is the only method until an online
// gateway lands; it is the DEFAULT so legacy rows and older app versions
// stay valid. A future gateway phase adds its own value (e.g. 'card')
// and confirms via webhook instead of at insert time.
const (
	PaymentMethodPayAtProperty = "pay_at_property"
)

// Booking is a hotel-room reservation shown in Booking History.
type Booking struct {
	ID         int64  `json:"id"`
	UserID     int64  `json:"user_id"`
	HotelID    int64  `json:"hotel_id"`
	RoomID     int64  `json:"room_id"` // 0 only for legacy rows whose room was deleted
	UnitNo     int    `json:"unit_no"` // which identical unit of the room type is held (1..total_units)
	HotelName  string `json:"hotel_name"`
	RoomName   string `json:"room_name"`
	Place      string `json:"place"`
	ImageURL   string `json:"image_url"`
	PriceMMK   int64  `json:"price_mmk"` // nightly rate snapshot at booking time
	Nights     int    `json:"nights"`
	TotalMMK   int64  `json:"total_mmk"` // price_mmk × nights
	GuestName  string `json:"guest_name,omitempty"`
	GuestPhone string `json:"guest_phone,omitempty"`
	CheckIn    string `json:"check_in"`  // YYYY-MM-DD, inclusive
	CheckOut   string `json:"check_out"` // YYYY-MM-DD, exclusive
	Guests     int    `json:"guests"`
	Status     string `json:"status"`
	// How the stay is paid: 'pay_at_property' (Phase 3 default — settle
	// at the front desk). Future gateway phases add their own value.
	PaymentMethod string     `json:"payment_method"`
	ConfirmedAt   *time.Time `json:"confirmed_at,omitempty"`
	CancelledAt   *time.Time `json:"cancelled_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at,omitempty"`
}

// CreateRequest is the POST /bookings body (user comes from the JWT).
// PaymentMethod is optional: omitted or 'pay_at_property' books instantly
// (born confirmed). Anything else is rejected until a gateway exists.
type CreateRequest struct {
	HotelID       int64  `json:"hotel_id" binding:"required"`
	RoomID        int64  `json:"room_id" binding:"required"`
	CheckIn       string `json:"check_in" binding:"required"`
	CheckOut      string `json:"check_out" binding:"required"`
	Guests        int    `json:"guests" binding:"min=1"`
	GuestName     string `json:"guest_name"`
	GuestPhone    string `json:"guest_phone"`
	PaymentMethod string `json:"payment_method"`
}

// RoomAvailability is one room's real-time state for a date window.
// UnitsBooked counts active (pending/confirmed) holds covering the
// WORST night of the window — a room is bookable only when every night
// still has a free unit.
type RoomAvailability struct {
	RoomID      int64  `json:"room_id"`
	Name        string `json:"name"`
	PriceMMK    int64  `json:"price_mmk"`
	Capacity    int    `json:"capacity"`
	ImageURL    string `json:"image_url"`
	Description string `json:"description,omitempty"`
	TotalUnits  int    `json:"total_units"`
	UnitsBooked int    `json:"units_booked"` // peak concurrent holds in the window
	UnitsFree   int    `json:"units_free"`   // TotalUnits - UnitsBooked, floored at 0
	IsActive    bool   `json:"is_active"`    // stop-sell switch (admin)
	Bookable    bool   `json:"bookable"`     // is_active && UnitsFree > 0
}

// AvailabilityResponse is the GET /hotels/:id/availability payload.
type AvailabilityResponse struct {
	HotelID  int64              `json:"hotel_id"`
	CheckIn  string             `json:"check_in"`
	CheckOut string             `json:"check_out"`
	Nights   int                `json:"nights"`
	Rooms    []RoomAvailability `json:"rooms"`
}

// RescheduleRequest is the PATCH /bookings/:id/reschedule body.
type RescheduleRequest struct {
	CheckIn  string `json:"check_in" binding:"required"`
	CheckOut string `json:"check_out" binding:"required"`
}
