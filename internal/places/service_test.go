package places

import (
	"context"
	"errors"
	"sync"
	"testing"

	"travel-app-backend/internal/pkg/external"

	"github.com/jackc/pgx/v5"
)

// fakeRepo is an in-memory Repository for service tests.
type fakeRepo struct {
	places []Place

	insertedPlaces []Place
	nextID         int64
}

func (f *fakeRepo) ListPlaces(context.Context, PlaceFilter) ([]Place, error) {
	return f.places, nil
}
func (f *fakeRepo) CountPlaces(context.Context, PlaceFilter) (int64, error) {
	return int64(len(f.places)), nil
}
func (f *fakeRepo) GetPlace(_ context.Context, id int64) (*Place, error) {
	for i := range f.places {
		if f.places[i].ID == id {
			return &f.places[i], nil
		}
	}
	return nil, pgx.ErrNoRows
}
func (f *fakeRepo) ListTrending(context.Context, int) ([]Place, error)       { return nil, nil }
func (f *fakeRepo) ListCategories(context.Context) ([]Category, error)       { return nil, nil }
func (f *fakeRepo) ListFoods(context.Context, bool) ([]Food, error)          { return nil, nil }
func (f *fakeRepo) ListFavorites(context.Context, int64) ([]Favorite, error) { return nil, nil }
func (f *fakeRepo) AddFavorite(context.Context, int64, int64, string) error  { return nil }
func (f *fakeRepo) RemoveFavorite(context.Context, int64, int64) error       { return pgx.ErrNoRows }

func (f *fakeRepo) InsertPlace(_ context.Context, p *Place) (int64, error) {
	f.nextID++
	p.ID = f.nextID
	f.insertedPlaces = append(f.insertedPlaces, *p)
	return p.ID, nil
}

// fakePlaceSearcher records calls; the mutex mirrors the real adapter's
// concurrency (seeds run all queries in parallel).
type fakePlaceSearcher struct {
	mu      sync.Mutex
	results []external.PlaceResult
	err     error
	calls   int
	queries []string
}

func (s *fakePlaceSearcher) SearchPlaces(_ context.Context, q string) ([]external.PlaceResult, error) {
	s.mu.Lock()
	s.calls++
	s.queries = append(s.queries, q)
	s.mu.Unlock()
	return s.results, s.err
}

// photoURL is a stand-in for a provider-native photo — the feed is
// photo-only, so fake provider results must carry one to survive caching.
const photoURL = "https://lh3/gmaps-photo=s1600"

// prose satisfies the curation bar (CuratedPlaceMedia) for non-Cities
// rows in tests.
const prose = "A sufficiently long description that satisfies the curation prose rule for places and foods in tests."

// nativePlace builds a provider result that passes the photo-only and
// curation gates.
func nativePlace(name string) external.PlaceResult {
	return external.PlaceResult{
		Name: name, Country: "France", Region: "Île-de-France", Category: "Parks",
		ImageURL: photoURL, Gallery: []string{"https://lh3/gmaps-extra=s1600"},
		Rating: 4.6, Description: prose, Lat: 48.85, Lon: 2.35,
	}
}

// Preset-data policy: ListPlaces is a pure database read — the runtime
// service has no searcher, and an empty database yields an empty list
// (no fallback, by design).
func TestListPlacesIsDBOnly(t *testing.T) {
	svc := NewService(&fakeRepo{})

	got, total, err := svc.ListPlaces(context.Background(), PlaceFilter{})
	if err != nil {
		t.Fatalf("ListPlaces: %v", err)
	}
	if len(got) != 0 || total != 0 {
		t.Fatalf("got %d results (total %d), want 0 on an empty database", len(got), total)
	}
}

// SeedExternal caches provider rows with their native photo, gallery and
// metadata — the bootstrap seeder's only write path.
func TestSeedExternalCachesNativeRows(t *testing.T) {
	repo := &fakeRepo{}
	searcher := &fakePlaceSearcher{results: []external.PlaceResult{nativePlace("Luxembourg Gardens")}}

	got := NewService(repo).SeedExternal(context.Background(), searcher, []string{"Paris parks"})

	if len(got) != 1 || len(repo.insertedPlaces) != 1 {
		t.Fatalf("seeded/served = %d/%d, want 1/1", len(repo.insertedPlaces), len(got))
	}
	p := repo.insertedPlaces[0]
	if p.Name != "Luxembourg Gardens" || p.Category != "Parks" {
		t.Errorf("row = %q/%q, want the provider identity", p.Name, p.Category)
	}
	if p.ImageURL != photoURL {
		t.Errorf("image = %q, want the provider-native photo untouched", p.ImageURL)
	}
	if len(p.GalleryURLs) != 1 || p.GalleryURLs[0] != "https://lh3/gmaps-extra=s1600" {
		t.Errorf("gallery = %v, want the provider gallery", p.GalleryURLs)
	}
	if p.Description != prose || p.Rating != 4.6 || p.Lat != 48.85 {
		t.Errorf("metadata not carried: %q/%v/%v", p.Description, p.Rating, p.Lat)
	}
	if searcher.calls != 1 {
		t.Errorf("searcher calls = %d, want 1", searcher.calls)
	}
}

// Photo-only feeds: a provider row without a photo never reaches Neon.
func TestSeedExternalDropsRowWithoutPhoto(t *testing.T) {
	repo := &fakeRepo{}
	noPhoto := nativePlace("Nameless Park")
	noPhoto.ImageURL = ""
	searcher := &fakePlaceSearcher{results: []external.PlaceResult{noPhoto}}

	got := NewService(repo).SeedExternal(context.Background(), searcher, []string{"Paris parks"})

	if len(got) != 0 || len(repo.insertedPlaces) != 0 {
		t.Errorf("cached = %d/%d, want 0/0 (rows without a photo are never cached)", len(repo.insertedPlaces), len(got))
	}
}

// Quality bar: thin (non-prose) descriptions fail curation for non-Cities
// rows and never reach Neon.
func TestSeedExternalCurationGate(t *testing.T) {
	repo := &fakeRepo{}
	thin := nativePlace("Thin Description Spot")
	thin.Description = "Paris, France"
	searcher := &fakePlaceSearcher{results: []external.PlaceResult{thin}}

	got := NewService(repo).SeedExternal(context.Background(), searcher, []string{"Paris parks"})

	if len(got) != 0 || len(repo.insertedPlaces) != 0 {
		t.Errorf("cached = %d/%d, want 0/0 (curation rejects thin rows)", len(repo.insertedPlaces), len(got))
	}
}

// Overlapping queries repeat a POI — the identity dedupe keeps one row.
func TestSeedExternalDedupesSameIdentity(t *testing.T) {
	repo := &fakeRepo{}
	searcher := &fakePlaceSearcher{results: []external.PlaceResult{nativePlace("Luxembourg Gardens")}}

	svc := NewService(repo)
	svc.SeedExternal(context.Background(), searcher, []string{"Paris parks", "Paris gardens"})

	// Both queries return the same identity, so exactly one row is cached.
	if len(repo.insertedPlaces) != 1 {
		t.Errorf("cached = %d, want 1 (identity dedupe)", len(repo.insertedPlaces))
	}
	if searcher.calls != 2 {
		t.Errorf("searcher calls = %d, want 2 (all queries run)", searcher.calls)
	}
}

// Provider failures during seeding degrade per query — logged, skipped,
// nothing cached, the run continues.
func TestSeedExternalDegradesOnProviderError(t *testing.T) {
	repo := &fakeRepo{}
	searcher := &fakePlaceSearcher{err: errors.Join(external.ErrProviderFailed, errors.New("rate limited (http 429)"))}

	got := NewService(repo).SeedExternal(context.Background(), searcher, []string{"Paris parks"})

	if len(got) != 0 || len(repo.insertedPlaces) != 0 {
		t.Errorf("cached on provider failure: %d served, %d cached, want 0/0", len(got), len(repo.insertedPlaces))
	}
	if searcher.calls != 1 {
		t.Errorf("searcher calls = %d, want 1 (a failed query must not abort the run)", searcher.calls)
	}
}

// A nil searcher (no -serpapi flag) seeds nothing and never panics.
func TestSeedExternalNilSearcher(t *testing.T) {
	repo := &fakeRepo{}

	got := NewService(repo).SeedExternal(context.Background(), nil, []string{"Paris parks"})

	if len(got) != 0 || len(repo.insertedPlaces) != 0 {
		t.Errorf("cached = %d/%d, want 0/0", len(repo.insertedPlaces), len(got))
	}
}
