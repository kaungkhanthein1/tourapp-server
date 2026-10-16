package places

import (
	"time"

	"travel-app-backend/internal/pkg/pagination"
)

// Place is a tourist attraction shown on Home/Explore/Detail screens.
type Place struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Country      string    `json:"country"`
	Region       string    `json:"region,omitempty"`
	Category     string    `json:"category"`
	ImageURL     string    `json:"image_url"`
	GalleryURLs  []string  `json:"gallery_urls"`
	Rating       float64   `json:"rating"`
	IsPopular    bool      `json:"is_popular"`
	IsTrending   bool      `json:"is_trending"`
	Description  string    `json:"description,omitempty"`
	CreatedAt    time.Time `json:"created_at,omitempty"`
}

// Category is an Explore-page filter chip.
type Category struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Icon      string `json:"icon,omitempty"`
	SortOrder int    `json:"sort_order"`
}

// Food is a local dish recommendation on the Home screen.
type Food struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Country     string `json:"country"`
	ImageURL    string `json:"image_url"`
	IsPopular   bool   `json:"is_popular"`
	Description string `json:"description,omitempty"`
}

// Favorite joins a user to a saved place (Favorites page).
type Favorite struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	PlaceID   int64     `json:"place_id"`
	Note      string    `json:"note,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	Place     *Place    `json:"place,omitempty"`
}

// PlaceFilter drives the Explore list query.
type PlaceFilter struct {
	Category   string
	Query      string
	IsPopular  *bool
	IsTrending *bool
	Page       pagination.Page
}

// AddFavoriteRequest is the POST /favorites body (user comes from the JWT).
type AddFavoriteRequest struct {
	PlaceID int64  `json:"place_id" binding:"required"`
	Note    string `json:"note"`
}
