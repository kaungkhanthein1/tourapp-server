package hotels

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"travel-app-backend/internal/pkg/external"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a hotel does not exist.
var ErrNotFound = errors.New("hotel not found")

// Service holds hotel business logic behind the Repository interface.
// Preset-data policy: the runtime never calls a hotel provider — every
// row comes from the bootstrap seeder's labeled runs (cmd/bootstrap
// -serpapi), which cache hotels and their rooms into Neon. ListHotels
// is a pure database read; provider failures during seeding degrade
// per query and never abort the run.
type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

func (s *Service) ListHotels(ctx context.Context, f Filter) ([]Hotel, int64, error) {
	list, err := s.repo.ListHotels(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.CountHotels(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// LabeledSeed is one preset-data seeding unit: Query runs against the
// provider (google_hotels text search), and every returned hotel is
// stored under Label — the exact places.name a place detail queries
// with — so the runtime place = $1 re-read matches. Region/Country
// override the query-derived values (the query text is search syntax,
// not row metadata).
type LabeledSeed struct {
	Label   string
	Query   string
	Region  string
	Country string
}

// SeedLabeled fetches each labeled query through the given searcher and
// caches the results (hotels + their rooms) into Neon — the bootstrap
// seeder's -serpapi run. Serial by design: the SerpApi client paces
// itself, and a per-seed failure is logged and skipped so the rest of
// the run still seeds.
func (s *Service) SeedLabeled(ctx context.Context, searcher external.HotelSearcher, seeds []LabeledSeed) []Hotel {
	if searcher == nil {
		return nil
	}
	var out []Hotel
	for _, sd := range seeds {
		res, err := searcher.SearchHotels(ctx, sd.Query)
		if err != nil {
			log.Printf("[hotels] seed query %q unavailable, skipping: %v", sd.Query, err)
			continue
		}
		for i := range res {
			res[i].Place = sd.Label
			if sd.Region != "" {
				res[i].Region = sd.Region
			}
			if sd.Country != "" {
				res[i].Country = sd.Country
			}
		}
		out = append(out, s.cacheHotelResults(ctx, res)...)
	}
	return out
}

// cacheHotelResults writes provider hotels (and, on first insert, their
// rooms) into Neon, deduping overlapping provider results by name|place.
// Provider-native photos (the SerpApi adapter's Google shots) are final;
// a row without a photo is dropped so placeholder cards never reach
// Neon (or the client). Cache-write failures never fail the request.
func (s *Service) cacheHotelResults(ctx context.Context, results []external.HotelResult) []Hotel {
	// Dedupe first: overlapping anchor circles can repeat a hotel.
	keep := make([]external.HotelResult, 0, len(results))
	seen := make(map[string]bool, len(results))
	for _, res := range results {
		key := strings.ToLower(res.Name + "|" + res.Place)
		if seen[key] {
			continue
		}
		seen[key] = true
		keep = append(keep, res)
	}

	var out []Hotel
	for _, res := range keep {
		if res.ImageURL == "" {
			// Photo-only hotel list: a provider row without a native
			// photo never reaches Neon.
			continue
		}
		image := res.ImageURL
		gallery := append([]string{}, res.Gallery...)
		// The first real gallery photo decorates the room card so it
		// differs from the hotel hero; without one the hero itself.
		roomImage := image
		for _, g := range gallery {
			if g != "" {
				roomImage = g
				break
			}
		}

		facilities := res.Facilities
		if facilities == nil {
			facilities = []string{} // facilities is NOT NULL — never bind a nil slice
		}
		h := Hotel{
			Name:        res.Name,
			Place:       res.Place,
			Country:     res.Country,
			Region:      res.Region,
			ImageURL:    image,
			GalleryURLs: gallery,
			Rating:      res.Rating,
			Facilities:  facilities,
		}
		id, created, ierr := s.repo.InsertHotel(ctx, &h)
		if ierr != nil {
			// Cache write failure must not fail the request — log and serve.
			log.Printf("[hotels] cache insert failed for %q, serving uncached result: %v", res.Name, ierr)
			out = append(out, h)
			continue
		}
		h.ID = id

		// Seed hotel_rooms only on first insert so repeat seed runs
		// never duplicate room rows for the same cached hotel. The room
		// card image is the first gallery photo (distinct from the hero;
		// falls back to the hero itself); a provider-native room photo
		// would be final.
		if created {
			for _, rm := range res.Rooms {
				roomPic := rm.ImageURL
				if roomPic == "" {
					roomPic = roomImage
				}
				room := Room{
					HotelID:     id,
					Name:        rm.Name,
					PriceMMK:    rm.PriceMMK,
					Capacity:    rm.Capacity,
					ImageURL:    roomPic,
					Description: rm.Description,
				}
				if _, rerr := s.repo.InsertRoom(ctx, &room); rerr != nil {
					log.Printf("[hotels] room cache insert failed for hotel %d (%s): %v", id, res.Name, rerr)
				}
			}
		}
		out = append(out, h)
	}
	return out
}

func (s *Service) GetHotel(ctx context.Context, id int64) (*Hotel, error) {
	h, err := s.repo.GetHotel(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get hotel %d: %w", id, err)
	}
	return h, nil
}

func (s *Service) ListRooms(ctx context.Context, hotelID int64) ([]Room, error) {
	if _, err := s.GetHotel(ctx, hotelID); err != nil {
		return nil, err
	}
	return s.repo.ListRooms(ctx, hotelID)
}
