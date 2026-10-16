package hotels

import (
	"context"
	"errors"
	"fmt"

	"travel-app-backend/internal/pkg/external"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a hotel does not exist.
var ErrNotFound = errors.New("hotel not found")

// Service holds hotel business logic behind the Repository interface.
// When a DB search misses for a place and a HotelSearcher (e.g. Makcorps)
// is configured, the service proxies the provider and caches results.
type Service struct {
	repo     Repository
	searcher external.HotelSearcher // optional hybrid fallback (nil = DB only)
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// WithSearcher wires an external hotel provider for DB-miss fallback.
func (s *Service) WithSearcher(searcher external.HotelSearcher) *Service {
	s.searcher = searcher
	return s
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

	// Hybrid fallback: empty DB result for a place → proxy external API.
	if len(list) == 0 && f.Place != "" && s.searcher != nil {
		fallback, ferr := s.fetchExternal(ctx, f.Place)
		if ferr == nil && len(fallback) > 0 {
			return fallback, int64(len(fallback)), nil
		}
	}

	return list, total, nil
}

// fetchExternal proxies the external searcher and caches hits into Neon.
func (s *Service) fetchExternal(ctx context.Context, place string) ([]Hotel, error) {
	results, err := s.searcher.SearchHotels(ctx, place)
	if err != nil {
		if errors.Is(err, external.ErrNotConfigured) || errors.Is(err, external.ErrProviderFailed) {
			return nil, nil // degrade to empty DB result
		}
		return nil, err
	}

	out := make([]Hotel, 0, len(results))
	for _, r := range results {
		h := Hotel{
			Name:        r.Name,
			Place:       r.Place,
			Country:     r.Country,
			ImageURL:    r.ImageURL,
			GalleryURLs: []string{},
			Rating:      r.Rating,
			Facilities:  []string{},
		}
		id, ierr := s.repo.InsertHotel(ctx, &h)
		if ierr == nil {
			h.ID = id
		}
		out = append(out, h)
	}
	return out, nil
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
