package hotels

import (
	"time"

	"travel-app-backend/internal/pkg/pagination"
)

// Hotel is a bookable property shown under a place's Hotels tab.
type Hotel struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Place       string    `json:"place"`
	Country     string    `json:"country"`
	Region      string    `json:"region,omitempty"`
	ImageURL    string    `json:"image_url"`
	GalleryURLs []string  `json:"gallery_urls"`
	Rating      float64   `json:"rating"`
	Facilities  []string  `json:"facilities"`
	PlaceID     *int64    `json:"place_id,omitempty"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
	// Rooms is populated on the hotel detail endpoint.
	Rooms []Room `json:"rooms,omitempty"`
}

// Room is a bookable room type with MMK pricing.
type Room struct {
	ID          int64  `json:"id"`
	HotelID     int64  `json:"hotel_id"`
	Name        string `json:"name"`
	PriceMMK    int64  `json:"price_mmk"`
	Capacity    int    `json:"capacity"`
	ImageURL    string `json:"image_url"`
	Description string `json:"description,omitempty"`
}

// Filter drives the hotel list query. Hotels are preset data only —
// Place matches the label a place detail seeds hotels under; Query is
// free text over the cached rows.
type Filter struct {
	Place string
	Query string
	Page  pagination.Page
}
