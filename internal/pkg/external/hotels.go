package external

import "context"

// HotelResult is the provider-neutral hotel shape cached into Neon.
type HotelResult struct {
	Name     string
	Place    string
	Country  string
	Region   string
	ImageURL string
	Rating   float64
	// Rooms carries parsed room types (or a synthesized example room
	// when the provider has no prices) for hotel_rooms caching.
	Rooms []RoomResult
	// Facilities and Gallery carry provider-native extras (the SerpApi
	// adapter): normalized amenity names for the hotel card's icon row,
	// and the photos beyond the hero.
	Facilities []string
	Gallery    []string
}

// RoomResult is the provider-neutral room shape cached into Neon.
type RoomResult struct {
	Name        string
	PriceMMK    int64
	Capacity    int
	ImageURL    string
	Description string
}

// HotelSearcher is the adapter interface consumed by the hotels Service
// at seed time only — the runtime never fetches hotels (preset-data
// policy): bootstrap walks labeled queries and caches them into Neon.
type HotelSearcher interface {
	SearchHotels(ctx context.Context, place string) ([]HotelResult, error)
}

const (
	// exampleRoomPriceMMK is the indicative nightly rate attached to the
	// synthesized Standard Room when a provider supplies no prices
	// (google_hotels rates always carry a price; the constant is the
	// synthetic fallback). It is NOT real pricing; the description says
	// so to the user.
	exampleRoomPriceMMK = 60000

	exampleRoomDescription = "Example rate — confirm pricing with the property before booking."
)
