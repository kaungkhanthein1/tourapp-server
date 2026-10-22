package hotels

import (
	"context"
	"errors"
	"sync"
	"testing"

	"travel-app-backend/internal/pkg/external"
	"travel-app-backend/internal/pkg/pagination"
)

// photoURL is a stand-in for a Commons/provider photo — the hotel list
// is photo-only, so fake provider results must carry one to survive caching.
const photoURL = "https://upload.wikimedia.org/wikipedia/commons/thumb/x/hotel.jpg"

// fakeRepo is an in-memory Repository for fallback tests. When hotels is
// nil it mirrors inserted rows on reads — the closest in-memory stand-in
// for "the provider results were cached into Neon and re-read".
type fakeRepo struct {
	hotels []Hotel
	rooms  []Room

	insertedHotels []Hotel
	insertedRooms  []Room
	nextID         int64
}

func (f *fakeRepo) readHotels() []Hotel {
	if f.hotels == nil {
		return f.insertedHotels
	}
	return f.hotels
}

func (f *fakeRepo) ListHotels(context.Context, Filter) ([]Hotel, error) {
	return f.readHotels(), nil
}
func (f *fakeRepo) CountHotels(context.Context, Filter) (int64, error) {
	return int64(len(f.readHotels())), nil
}
func (f *fakeRepo) GetHotel(_ context.Context, id int64) (*Hotel, error) {
	for i := range f.hotels {
		if f.hotels[i].ID == id {
			return &f.hotels[i], nil
		}
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) ListRooms(_ context.Context, hotelID int64) ([]Room, error) {
	var out []Room
	for _, rm := range f.rooms {
		if rm.HotelID == hotelID {
			out = append(out, rm)
		}
	}
	return out, nil
}

func (f *fakeRepo) InsertHotel(_ context.Context, h *Hotel) (int64, bool, error) {
	for _, existing := range f.insertedHotels {
		if existing.Name == h.Name && existing.Place == h.Place {
			return existing.ID, false, nil
		}
	}
	f.nextID++
	h.ID = f.nextID
	f.insertedHotels = append(f.insertedHotels, *h)
	return h.ID, true, nil
}

func (f *fakeRepo) InsertRoom(_ context.Context, rm *Room) (int64, error) {
	f.nextID++
	rm.ID = f.nextID
	f.insertedRooms = append(f.insertedRooms, *rm)
	return rm.ID, nil
}

type fakeHotelSearcher struct {
	mu      sync.Mutex
	results []external.HotelResult
	err     error
	calls   int
	places  []string
}

func (s *fakeHotelSearcher) SearchHotels(_ context.Context, place string) ([]external.HotelResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.places = append(s.places, place)
	return s.results, s.err
}

func (s *fakeHotelSearcher) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fakeHotelSearcher) callPlaces() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.places...)
}

// Preset-data policy: ListHotels is a pure database read — the runtime
// service has no searcher, and an empty database yields an empty list
// (no fallback, by design).
func TestListHotelsIsDBOnly(t *testing.T) {
	svc := NewService(&fakeRepo{})

	got, total, err := svc.ListHotels(context.Background(), Filter{Place: "yangon", Page: pagination.Page{Limit: 20}})
	if err != nil {
		t.Fatalf("ListHotels: %v", err)
	}
	if len(got) != 0 || total != 0 {
		t.Fatalf("got %d results (total %d), want 0 on an empty database", len(got), total)
	}
}

// SeedLabeled pins provider results to the seed's label — the exact
// places.name the place detail queries with — overriding whatever the
// query text derived, and caches the hotels with their rooms.
func TestSeedLabeledRelabelsResults(t *testing.T) {
	repo := &fakeRepo{}
	searcher := &fakeHotelSearcher{results: []external.HotelResult{
		{
			Name: "Hôtel de Londres Eiffel", Place: "hotels near Eiffel Tower",
			Country: "Paris, France", Region: "hotels near Eiffel Tower",
			Rating: 4.6, ImageURL: photoURL,
			Rooms: []external.RoomResult{{Name: "Standard Room", PriceMMK: 445200, Capacity: 2}},
		},
	}}
	svc := NewService(repo)

	got := svc.SeedLabeled(context.Background(), searcher, []LabeledSeed{{
		Label: "Eiffel Tower", Query: "hotels near Eiffel Tower, Paris, France",
		Region: "Paris", Country: "France",
	}})
	if len(got) != 1 || len(repo.insertedHotels) != 1 {
		t.Fatalf("seeded/served = %d/%d, want 1/1", len(repo.insertedHotels), len(got))
	}
	h := repo.insertedHotels[0]
	if h.Place != "Eiffel Tower" {
		t.Errorf("Place = %q, want the exact label the place detail queries with", h.Place)
	}
	if h.Region != "Paris" || h.Country != "France" {
		t.Errorf("Region/Country = %q/%q, want the seed overrides (query-derived values are syntax, not metadata)", h.Region, h.Country)
	}
	if len(repo.insertedRooms) != 1 || repo.insertedRooms[0].PriceMMK != 445200 {
		t.Errorf("rooms = %+v, want the provider-priced room", repo.insertedRooms)
	}
	if p := searcher.callPlaces(); len(p) != 1 || p[0] != "hotels near Eiffel Tower, Paris, France" {
		t.Errorf("queries = %v, want the seed's query text verbatim", p)
	}
}

// A repeat seed run is a get-or-create: same label+name never
// duplicates hotels or rooms — reruns only pay for what is missing.
func TestSeedLabeledIsIdempotent(t *testing.T) {
	repo := &fakeRepo{}
	searcher := &fakeHotelSearcher{results: []external.HotelResult{
		{Name: "Repeat Hotel", Place: "yangon", ImageURL: photoURL,
			Rooms: []external.RoomResult{{Name: "Room", PriceMMK: 1000}}},
	}}
	svc := NewService(repo)
	seeds := []LabeledSeed{{Label: "yangon", Query: "hotels near Yangon, Myanmar"}}

	svc.SeedLabeled(context.Background(), searcher, seeds)
	svc.SeedLabeled(context.Background(), searcher, seeds)
	if len(repo.insertedHotels) != 1 || len(repo.insertedRooms) != 1 {
		t.Errorf("cached hotels/rooms = %d/%d, want 1/1 (get-or-create)", len(repo.insertedHotels), len(repo.insertedRooms))
	}
	if searcher.callCount() != 2 {
		t.Errorf("searcher calls = %d, want 2 (seeding always queries; dedupe happens at insert)", searcher.callCount())
	}
}

// Provider failures during seeding degrade per seed — logged, skipped,
// nothing cached, the run continues.
func TestSeedLabeledDegradesOnProviderError(t *testing.T) {
	repo := &fakeRepo{}
	searcher := &fakeHotelSearcher{err: errors.Join(external.ErrProviderFailed, errors.New("rate limited (http 429)"))}
	svc := NewService(repo)

	got := svc.SeedLabeled(context.Background(), searcher, []LabeledSeed{{Label: "yangon", Query: "hotels near Yangon, Myanmar"}})
	if len(got) != 0 || len(repo.insertedHotels) != 0 {
		t.Errorf("seeded on provider failure: %d served, %d cached, want 0/0", len(got), len(repo.insertedHotels))
	}
	if searcher.callCount() != 1 {
		t.Errorf("searcher calls = %d, want 1 (a failed seed must not abort the run)", searcher.callCount())
	}
}

// cacheHotelResults keeps provider-native photos final: the first
// gallery photo distinct from the hero decorates the room card, and the
// gallery rides along untouched.
func TestCacheHotelResultsRoomGetsGalleryPhoto(t *testing.T) {
	repo := &fakeRepo{}
	results := []external.HotelResult{
		{Name: "Hôtel Notre-Dame", Place: "paris", Country: "France",
			ImageURL: "https://lh3/hotel-hero=s1600",
			Gallery:  []string{"https://lh3/hotel-room", "https://lh3/hotel-extra=s1600"},
			Rooms:    []external.RoomResult{{Name: "Standard Room", PriceMMK: 60000, Capacity: 2, ImageURL: "https://lh3/provider-room"}}},
	}
	svc := NewService(repo)

	svc.cacheHotelResults(context.Background(), results)
	if len(repo.insertedHotels) != 1 {
		t.Fatalf("hotels cached = %d, want 1", len(repo.insertedHotels))
	}
	h := repo.insertedHotels[0]
	if h.ImageURL != "https://lh3/hotel-hero=s1600" {
		t.Errorf("hotel image = %q, want the provider hero untouched", h.ImageURL)
	}
	if len(h.GalleryURLs) != 2 {
		t.Errorf("gallery = %v, want both provider photos", h.GalleryURLs)
	}
	if len(repo.insertedRooms) != 1 {
		t.Fatalf("rooms cached = %d, want 1", len(repo.insertedRooms))
	}
	if repo.insertedRooms[0].ImageURL != "https://lh3/provider-room" {
		t.Errorf("room image = %q, want the provider-native room photo", repo.insertedRooms[0].ImageURL)
	}
}

// A provider row without a photo never reaches Neon — the list is
// photo-only, so placeholder cards are dropped at cache time.
func TestCacheHotelResultsDropsRowWithoutPhoto(t *testing.T) {
	repo := &fakeRepo{}
	results := []external.HotelResult{
		{Name: "Backpacker Inn", Place: "yangon", Country: "Myanmar",
			ImageURL: "",
			Rooms:    []external.RoomResult{{Name: "Standard Room", PriceMMK: 60000, Capacity: 2}}},
	}
	svc := NewService(repo)

	svc.cacheHotelResults(context.Background(), results)
	if len(repo.insertedHotels) != 0 || len(repo.insertedRooms) != 0 {
		t.Errorf("cached hotels/rooms = %d/%d, want 0/0 (rows without a photo are never cached)",
			len(repo.insertedHotels), len(repo.insertedRooms))
	}
}
