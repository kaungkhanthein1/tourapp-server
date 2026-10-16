package bookings

import "time"

// Status values for a booking (Pending/Complete tabs in Figma).
const (
	StatusPending  = "pending"
	StatusComplete = "complete"
	StatusCancelled = "cancelled"
)

// Booking is a hotel-room reservation shown in Booking History.
type Booking struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	HotelID   int64     `json:"hotel_id"`
	HotelName string    `json:"hotel_name"`
	RoomName  string    `json:"room_name"`
	Place     string    `json:"place"`
	ImageURL  string    `json:"image_url"`
	PriceMMK  int64     `json:"price_mmk"`
	CheckIn   string    `json:"check_in"`
	CheckOut  string    `json:"check_out"`
	Guests    int       `json:"guests"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// CreateRequest is the POST /bookings body (user comes from the JWT).
type CreateRequest struct {
	HotelID  int64  `json:"hotel_id" binding:"required"`
	RoomID   int64  `json:"room_id" binding:"required"`
	CheckIn  string `json:"check_in" binding:"required"`
	CheckOut string `json:"check_out" binding:"required"`
	Guests   int    `json:"guests" binding:"min=1"`
}

// RescheduleRequest is the PATCH /bookings/:id/reschedule body.
type RescheduleRequest struct {
	CheckIn  string `json:"check_in" binding:"required"`
	CheckOut string `json:"check_out" binding:"required"`
}
