package places

import (
	"context"
	"errors"
	"fmt"

	"travel-app-backend/internal/pkg/external"

	"github.com/jackc/pgx/v5"
)

// ErrNotFound is returned when a place does not exist.
var ErrNotFound = errors.New("place not found")

// ErrFavoriteNotFound is returned when removing a non-existent favorite.
var ErrFavoriteNotFound = errors.New("favorite not found")

// Service contains place/food/favorite business logic.
// It depends only on the Repository interface, so an external API
// repository can be injected without changing handlers.
// When a DB search misses and a PlaceSearcher (e.g. Google Places) is
// configured, the service proxies the provider, caches results into Neon,
// and returns them in the standard envelope.
type Service struct {
	repo    Repository
	searcher external.PlaceSearcher // optional hybrid fallback (nil = DB only)
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// WithSearcher wires an external place provider for DB-miss fallback.
func (s *Service) WithSearcher(searcher external.PlaceSearcher) *Service {
	s.searcher = searcher
	return s
}

func (s *Service) ListPlaces(ctx context.Context, f PlaceFilter) ([]Place, int64, error) {
	list, err := s.repo.ListPlaces(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	total, err := s.repo.CountPlaces(ctx, f)
	if err != nil {
		return nil, 0, err
	}

	// Hybrid fallback: empty DB result + search query → proxy external API.
	if len(list) == 0 && f.Query != "" && s.searcher != nil {
		fallback, ferr := s.fetchExternal(ctx, f.Query)
		if ferr == nil && len(fallback) > 0 {
			return fallback, int64(len(fallback)), nil
		}
	}

	return list, total, nil
}

// fetchExternal proxies the external searcher and caches hits into Neon.
func (s *Service) fetchExternal(ctx context.Context, query string) ([]Place, error) {
	results, err := s.searcher.SearchPlaces(ctx, query)
	if err != nil {
		if errors.Is(err, external.ErrNotConfigured) || errors.Is(err, external.ErrProviderFailed) {
			return nil, nil // degrade to empty DB result
		}
		return nil, err
	}

	out := make([]Place, 0, len(results))
	for _, r := range results {
		p := Place{
			Name:        r.Name,
			Country:     r.Country,
			Region:      r.Region,
			Category:    r.Category,
			ImageURL:    r.ImageURL,
			GalleryURLs: []string{},
			Rating:      r.Rating,
			Description: r.Description,
		}
		id, ierr := s.repo.InsertPlace(ctx, &p)
		if ierr == nil {
			p.ID = id
		}
		// On cache race (duplicate) still return the mapped record.
		out = append(out, p)
	}
	return out, nil
}

func (s *Service) GetPlace(ctx context.Context, id int64) (*Place, error) {
	p, err := s.repo.GetPlace(ctx, id)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		// pgx returns ErrNoRows for missing rows
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get place %d: %w", id, err)
	}
	return p, nil
}

func (s *Service) ListTrending(ctx context.Context, limit int) ([]Place, error) {
	return s.repo.ListTrending(ctx, limit)
}

func (s *Service) ListCategories(ctx context.Context) ([]Category, error) {
	return s.repo.ListCategories(ctx)
}

func (s *Service) ListFoods(ctx context.Context, popularOnly bool) ([]Food, error) {
	return s.repo.ListFoods(ctx, popularOnly)
}

func (s *Service) ListFavorites(ctx context.Context, userID int64) ([]Favorite, error) {
	return s.repo.ListFavorites(ctx, userID)
}

func (s *Service) AddFavorite(ctx context.Context, userID, placeID int64, note string) error {
	if _, err := s.repo.GetPlace(ctx, placeID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	return s.repo.AddFavorite(ctx, userID, placeID, note)
}

func (s *Service) RemoveFavorite(ctx context.Context, userID, placeID int64) error {
	err := s.repo.RemoveFavorite(ctx, userID, placeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFavoriteNotFound
	}
	return err
}
