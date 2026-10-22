package places

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

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
// Data is preset-only: the repository serves Neon (the seeded mock rows
// are hidden by source='seed'), and nothing is fetched at runtime —
// bootstrap's -serpapi run is the only writer (SeedExternal).
type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
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
	return list, total, nil
}

// SeedExternal forces the provider fetch path for the given queries even
// when the table already holds rows (ListPlaces would otherwise serve
// Neon and never fetch) — the bootstrap seeder's -serpapi run against a
// warm database. The searcher is passed explicitly so the seeder can
// target SerpApi; nothing else in the service fetches.
func (s *Service) SeedExternal(ctx context.Context, searcher external.PlaceSearcher, queries []string) []Place {
	if searcher == nil {
		return nil
	}
	type providerResult struct {
		query   string
		results []external.PlaceResult
		err     error
	}
	results := make([]providerResult, len(queries))
	var wg sync.WaitGroup
	for i, q := range queries {
		wg.Add(1)
		go func(i int, q string) {
			defer wg.Done()
			res, err := searcher.SearchPlaces(ctx, q)
			results[i] = providerResult{query: q, results: res, err: err}
		}(i, q)
	}
	wg.Wait()

	var out []Place
	seen := make(map[string]bool, 16) // overlapping anchor circles can repeat a POI
	for _, r := range results {
		if r.err != nil {
			log.Printf("[places] external search %q unavailable, degrading to database data: %v", r.query, r.err)
			continue
		}
		for _, res := range r.results {
			key := strings.ToLower(res.Name + "|" + res.Country)
			if seen[key] {
				continue
			}
			seen[key] = true
			if res.ImageURL == "" {
				// Photo-only feeds: a provider row without a native photo
				// never reaches Neon (placeholder cards are dropped).
				continue
			}
			if !CuratedPlaceMedia(res.Category, res.ImageURL, res.Description) {
				// Quality bar: junk/map/postcard-era file titles, chip
				// mismatches, and thin (non-prose) rows never reach Neon.
				continue
			}
			gallery := res.Gallery
			if gallery == nil {
				gallery = []string{} // gallery_urls is NOT NULL — never bind a nil slice
			}
			p := Place{
				Name:        res.Name,
				Country:     res.Country,
				Region:      res.Region,
				Category:    res.Category,
				ImageURL:    res.ImageURL,
				GalleryURLs: gallery,
				Rating:      res.Rating,
				Description: res.Description,
				Lat:         res.Lat,
				Lon:         res.Lon,
			}
			id, ierr := s.repo.InsertPlace(ctx, &p)
			if ierr != nil {
				// Cache write failure must not fail the request — log and serve.
				log.Printf("[places] cache insert failed for %q, serving uncached result: %v", p.Name, ierr)
			} else {
				p.ID = id
			}
			out = append(out, p)
		}
	}
	return out
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
