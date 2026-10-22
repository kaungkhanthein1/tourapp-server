package external

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testSerpapiClient wires a SerpAPIClient to a stub server (no pacing,
// fixed key) — the base URL points at the stub, the default plan stays
// untouched.
func testSerpapiClient(t *testing.T, handler http.HandlerFunc) *SerpAPIClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := NewSerpAPIClient("test-key")
	c.baseURL = srv.URL
	c.minInterval = 0
	return c
}

// reqCapture records request details from handler goroutines safely for
// assertions after the call returns.
type reqCapture struct {
	mu sync.Mutex
	q  url.Values
	ua string
}

func (c *reqCapture) save(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.q = r.URL.Query()
	c.ua = r.Header.Get("User-Agent")
}

func (c *reqCapture) get() (url.Values, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.q, c.ua
}

const mapsFixture = `{
  "search_metadata": {"status": "Success"},
  "local_results": [
    {
      "position": 1,
      "title": "Arc de Triomphe",
      "description": "Triumphal arch & national monument with an iconic terrace overlooking the Champs-Élysées.",
      "address": "Pl. Charles de Gaulle, 75008 Paris, France",
      "country": "France",
      "rating": 4.7,
      "reviews": 301269,
      "type": "Tourist attraction",
      "types": ["Tourist attraction", "Cultural landmark", "Monument"],
      "gps_coordinates": {"latitude": 48.8737, "longitude": 2.295},
      "thumbnail": "https://lh3.googleusercontent.com/gp-photo-arc=w1000-h1000-c-n"
    },
    {
      "position": 2,
      "title": "Luxembourg Gardens",
      "description": "Formal garden with tree-lined promenades, fountains and a playground in the 6th arrondissement of Paris.",
      "address": "Rue de Vaugirard, 75006 Paris, France",
      "country": "France",
      "rating": 4.6,
      "type": "Park",
      "types": ["Park", "Garden"],
      "gps_coordinates": {"latitude": 48.846, "longitude": 2.337},
      "thumbnail": "https://lh3.googleusercontent.com/gp-photo-lux=s600"
    },
    {
      "position": 3,
      "title": "Pinky Tour - Paris",
      "description": "Guided city tours by vintage sidecar with champagne.",
      "address": "12 Rue de Rivoli, 75001 Paris, France",
      "country": "France",
      "type": "Sightseeing tour agency",
      "types": ["Sightseeing tour agency", "Transportation escort service"],
      "thumbnail": "https://lh3.googleusercontent.com/gp-photo-tour=w1000-h1000-c-n"
    }
  ]
}`

func TestSearchPlacesMapsLocalResults(t *testing.T) {
	var cap reqCapture
	c := testSerpapiClient(t, func(w http.ResponseWriter, r *http.Request) {
		cap.save(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(mapsFixture))
	})

	got, err := c.SearchPlaces(context.Background(), "Paris attractions")
	if err != nil {
		t.Fatalf("SearchPlaces: %v", err)
	}
	// The tour-agency row is not a destination — dropped, not mapped.
	if len(got) != 2 {
		t.Fatalf("results = %d, want 2 (service listing dropped)", len(got))
	}

	q, ua := cap.get()
	if ua != serpapiUserAgent {
		t.Errorf("User-Agent = %q, want %q", ua, serpapiUserAgent)
	}
	for key, want := range map[string]string{
		"engine":  "google_maps",
		"type":    "search",
		"q":       "Paris attractions",
		"hl":      "en",
		"api_key": "test-key",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("param %s = %q, want %q", key, got, want)
		}
	}

	arc := got[0]
	if arc.Name != "Arc de Triomphe" || arc.Country != "France" || arc.Region != "Paris" {
		t.Errorf("arc identity = %q/%q/%q, want Arc de Triomphe/France/Paris", arc.Name, arc.Country, arc.Region)
	}
	if arc.Category != "Architecture" {
		t.Errorf("arc category = %q, want Architecture", arc.Category)
	}
	if want := "https://lh3.googleusercontent.com/gp-photo-arc=s1600"; arc.ImageURL != want {
		t.Errorf("arc image = %q, want %q (w1000-h1000 render upsized)", arc.ImageURL, want)
	}
	if arc.Rating != 4.7 || arc.Lat != 48.8737 || arc.Lon != 2.295 {
		t.Errorf("arc rating/coords = %v/%v/%v, want 4.7/48.8737/2.295", arc.Rating, arc.Lat, arc.Lon)
	}
	if len(arc.Description) < MinCuratedProseLenProbe {
		t.Errorf("arc description too short: %q", arc.Description)
	}
	if arc.Gallery != nil {
		t.Errorf("arc gallery = %v, want nil (google_maps ships one thumbnail)", arc.Gallery)
	}

	gardens := got[1]
	if gardens.Category != "Parks" {
		t.Errorf("gardens category = %q, want Parks", gardens.Category)
	}
	if gardens.Region != "Paris" {
		t.Errorf("gardens region = %q, want Paris", gardens.Region)
	}
	if want := "https://lh3.googleusercontent.com/gp-photo-lux=s1600"; gardens.ImageURL != want {
		t.Errorf("gardens image = %q, want %q (s600 upsized)", gardens.ImageURL, want)
	}
}

// MinCuratedProseLenProbe mirrors the places package's prose bar for
// this test without importing it (the gate itself runs in the service).
const MinCuratedProseLenProbe = 80

func TestSearchPlacesCountryFallsBackToAddressTail(t *testing.T) {
	c := testSerpapiClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"search_metadata":{"status":"Success"},"local_results":[
			{"title":"Sagrada Família","address":"Sagrada Família, 08013 Barcelona, Spain",
			 "type":"Basilica","types":["Basilica","Cathedral"],
			 "gps_coordinates":{"latitude":41.4036,"longitude":2.1744},
			 "thumbnail":"https://lh3.googleusercontent.com/sagrada"}
		]}`))
	})

	got, err := c.SearchPlaces(context.Background(), "Barcelona attractions")
	if err != nil {
		t.Fatalf("SearchPlaces: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("results = %d, want 1", len(got))
	}
	if got[0].Country != "Spain" || got[0].Region != "Barcelona" {
		t.Errorf("country/region = %q/%q, want Spain/Barcelona (address tail + postcode strip)", got[0].Country, got[0].Region)
	}
	if got[0].Category != "Architecture" {
		t.Errorf("category = %q, want Architecture (basilica fallback)", got[0].Category)
	}
	if got[0].ImageURL != "https://lh3.googleusercontent.com/sagrada" {
		t.Errorf("bare image must ship untouched, got %q", got[0].ImageURL)
	}
}

func TestClassifyPlace(t *testing.T) {
	tests := []struct {
		name  string
		kind  string
		types []string
		want  string
	}{
		{"monument lands in architecture", "Tourist attraction", []string{"Cathedral", "Monument"}, "Architecture"},
		{"garden lands in parks", "Park", []string{"Garden"}, "Parks"},
		{"city park stays parks", "City park", []string{"Park"}, "Parks"},
		{"beach lands in beaches", "Beach", []string{"Public beach"}, "Beaches"},
		{"volcano lands in hills", "Tourist attraction", []string{"Volcano"}, "Hills"},
		{"literal city entry", "City", []string{"City"}, "Cities"},
		{"town entry", "Town hall", []string{"Town"}, "Cities"},
		{"restaurant lands in foods", "Restaurant", []string{"Asian restaurant"}, "Foods"},
		{"street food market lands in foods", "Street food market", []string{"Market"}, "Foods"},
		{"unmapped sight falls back to architecture", "Promenade", []string{"Promenade"}, "Architecture"},
		{"tour agency dropped", "Sightseeing tour agency", nil, ""},
		{"photo service dropped", "Photography service", nil, ""},
		{"escort service dropped", "Transportation escort service", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyPlace(tt.kind, tt.types); got != tt.want {
				t.Errorf("classifyPlace(%q, %v) = %q, want %q", tt.kind, tt.types, got, tt.want)
			}
		})
	}
}

// The photo rewrite rules were fixed by direct fetches against live
// Google image URLs (2026-10-21): size tokens accept =s1600, bare URLs
// and unknown tokens must never be touched.
func TestScaleGooglePhoto(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"s10000 upsized", "https://lh3/x=s10000", "https://lh3/x=s1600"},
		{"crop pair upsized", "https://lh3/x=w1000-h1000-c-n", "https://lh3/x=s1600"},
		{"small numeric 80 upsized", "https://lh3/x=80", "https://lh3/x=s1600"},
		{"medium upsized", "https://lh3/x=medium", "https://lh3/x=s1600"},
		{"bare url untouched", "https://lh3/xphoto", "https://lh3/xphoto"},
		{"one untouched", "https://lh3/x=1", "https://lh3/x=1"},
		{"auth token untouched", "https://lh3/x=AQABAAAabc", "https://lh3/x=AQABAAAabc"},
		{"compound token untouched", "https://lh3/x=s287-w287-h192-n-k-no-v1", "https://lh3/x=s287-w287-h192-n-k-no-v1"},
		{"empty stays empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scaleGooglePhoto(tt.in); got != tt.want {
				t.Errorf("scaleGooglePhoto(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSerpapiCountryRegion(t *testing.T) {
	tests := []struct {
		address, country string
		wantRegion       string
		wantCountry      string
	}{
		{"Pl. Charles de Gaulle, 75008 Paris, France", "France", "Paris", "France"},
		{"Paris, France", "France", "Paris", "France"},
		{"Sagrada Família, 08013 Barcelona, Spain", "", "Barcelona", "Spain"},
		{"6 Parvis Notre-Dame - Pl. Jean-Paul II, 75004 Paris, France", "France", "Paris", "France"},
		{"", "France", "", "France"},
	}
	for _, tt := range tests {
		region, country := serpapiCountryRegion(tt.address, tt.country)
		if region != tt.wantRegion || country != tt.wantCountry {
			t.Errorf("serpapiCountryRegion(%q, %q) = (%q, %q), want (%q, %q)",
				tt.address, tt.country, region, country, tt.wantRegion, tt.wantCountry)
		}
	}
}

func TestSplitAnchorQuery(t *testing.T) {
	tests := []struct {
		in          string
		wantAnchor  string
		wantCountry string
	}{
		{"Paris, France", "Paris", "France"},
		{"Tokyo", "Tokyo", ""},
		{"  Bangkok ,  Thailand ", "Bangkok", "Thailand"},
	}
	for _, tt := range tests {
		anchor, country := splitAnchorQuery(tt.in)
		if anchor != tt.wantAnchor || country != tt.wantCountry {
			t.Errorf("splitAnchorQuery(%q) = (%q, %q), want (%q, %q)",
				tt.in, anchor, country, tt.wantAnchor, tt.wantCountry)
		}
	}
}

const hotelsFixture = `{
  "search_metadata": {"status": "Success"},
  "properties": [
    {
      "type": "hotel",
      "name": "The Neuville hotel Paris",
      "overall_rating": 4.4,
      "rate_per_night": {"lowest": "$98", "extracted_lowest": 98},
      "amenities": ["Breakfast ($)", "Free Wi-Fi", "Fitness center", "Parking ($)",
                    "Bar", "Room service", "Spa", "Indoor pool", "Air conditioning", "Kid-friendly"],
      "gps_coordinates": {"latitude": 48.887, "longitude": 2.3006},
      "images": [
        {"thumbnail": "t", "original_image": "https://lh3/hotel1=s10000"},
        {"thumbnail": "t", "original_image": "https://lh3/hotel2"},
        {"thumbnail": "t", "original_image": "https://lh3/hotel3=80"},
        {"thumbnail": "t", "original_image": "https://lh3/hotel1=s10000"}
      ]
    },
    {
      "type": "vacation rental",
      "name": "Canal Loft",
      "overall_rating": 4.7
    }
  ]
}`

func TestSearchHotelsMapsProperties(t *testing.T) {
	var cap reqCapture
	c := testSerpapiClient(t, func(w http.ResponseWriter, r *http.Request) {
		cap.save(r)
		_, _ = w.Write([]byte(hotelsFixture))
	})

	got, err := c.SearchHotels(context.Background(), "Paris, France")
	if err != nil {
		t.Fatalf("SearchHotels: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("results = %d, want 2", len(got))
	}

	q, ua := cap.get()
	if ua != serpapiUserAgent {
		t.Errorf("User-Agent = %q, want %q", ua, serpapiUserAgent)
	}
	for key, want := range map[string]string{
		"engine":   "google_hotels",
		"q":        "Paris, France",
		"gl":       "us",
		"hl":       "en",
		"currency": "USD",
		"adults":   "2",
		"children": "0",
		"api_key":  "test-key",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("param %s = %q, want %q", key, got, want)
		}
	}
	checkIn, err := time.Parse("2006-01-02", q.Get("check_in_date"))
	if err != nil {
		t.Fatalf("check_in_date %q: %v", q.Get("check_in_date"), err)
	}
	checkOut, err := time.Parse("2006-01-02", q.Get("check_out_date"))
	if err != nil {
		t.Fatalf("check_out_date %q: %v", q.Get("check_out_date"), err)
	}
	if d := checkOut.Sub(checkIn); d != 48*time.Hour {
		t.Errorf("stay window = %v, want 2 nights", d)
	}

	h := got[0]
	if h.Name != "The Neuville hotel Paris" || h.Place != "Paris" || h.Country != "France" || h.Region != "Paris" {
		t.Errorf("hotel identity = %q/%q/%q/%q, want …/Paris/France/Paris", h.Name, h.Place, h.Country, h.Region)
	}
	if h.Rating != 4.4 {
		t.Errorf("rating = %v, want 4.4", h.Rating)
	}
	if want := "https://lh3/hotel1=s1600"; h.ImageURL != want {
		t.Errorf("hero = %q, want %q (first image, upsized)", h.ImageURL, want)
	}
	// Gallery: extra photos in order (bare untouched, 80 upsized, hero
	// duplicate skipped).
	if len(h.Gallery) != 2 {
		t.Fatalf("gallery = %v, want [hotel2, hotel3=s1600]", h.Gallery)
	}
	if h.Gallery[0] != "https://lh3/hotel2" || h.Gallery[1] != "https://lh3/hotel3=s1600" {
		t.Errorf("gallery photos = %v", h.Gallery)
	}
	wantFacilities := []string{"Breakfast", "Free Wifi", "Gym", "Parking", "Bar", "Room service", "Spa", "Swimming pool"}
	if len(h.Facilities) != len(wantFacilities) {
		t.Fatalf("facilities = %v, want %v (normalized, capped at 8)", h.Facilities, wantFacilities)
	}
	for i, f := range wantFacilities {
		if h.Facilities[i] != f {
			t.Errorf("facilities[%d] = %q, want %q", i, h.Facilities[i], f)
		}
	}
	if len(h.Rooms) != 1 {
		t.Fatalf("rooms = %d, want 1 (property-level rate)", len(h.Rooms))
	}
	room := h.Rooms[0]
	if room.Name != "Standard Room" || room.Capacity != 2 {
		t.Errorf("room = %+v, want Standard Room/2", room)
	}
	if want := int64(98 * usdToMMKRate); room.PriceMMK != want {
		t.Errorf("room price = %d, want %d (US$98 × %d)", room.PriceMMK, want, int64(usdToMMKRate))
	}
	for _, frag := range []string{"US$98", "205800", "2100", "Indicative"} {
		if !strings.Contains(room.Description, frag) {
			t.Errorf("room description %q missing %q", room.Description, frag)
		}
	}

	// Rate-less property: shared example room, facilities stay non-nil.
	r := got[1]
	if r.Name != "Canal Loft" || r.Place != "Paris" {
		t.Errorf("second hotel = %q/%q, want Canal Loft/Paris", r.Name, r.Place)
	}
	if r.ImageURL != "" {
		t.Errorf("hero = %q, want empty (no images)", r.ImageURL)
	}
	if r.Facilities == nil || len(r.Facilities) != 0 {
		t.Errorf("facilities = %v, want non-nil empty slice", r.Facilities)
	}
	if len(r.Rooms) != 1 {
		t.Fatalf("rooms = %d, want 1", len(r.Rooms))
	}
	if r.Rooms[0].PriceMMK != exampleRoomPriceMMK || r.Rooms[0].Description != exampleRoomDescription {
		t.Errorf("fallback room = %+v, want the shared example room", r.Rooms[0])
	}
}

func TestSearchHotelsWithoutCountryAnchor(t *testing.T) {
	c := testSerpapiClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"search_metadata":{"status":"Success"},"properties":[
			{"name":"Shinjuku Grand","overall_rating":4.5,"rate_per_night":{"extracted_lowest":150}}
		]}`))
	})

	got, err := c.SearchHotels(context.Background(), "Tokyo")
	if err != nil {
		t.Fatalf("SearchHotels: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("results = %d, want 1", len(got))
	}
	if got[0].Place != "Tokyo" || got[0].Country != "" {
		t.Errorf("place/country = %q/%q, want Tokyo/empty", got[0].Place, got[0].Country)
	}
	if want := int64(150 * usdToMMKRate); got[0].Rooms[0].PriceMMK != want {
		t.Errorf("price = %d, want %d", got[0].Rooms[0].PriceMMK, want)
	}
}

func TestNormalizeAmenities(t *testing.T) {
	if out := normalizeAmenities(nil); out == nil || len(out) != 0 {
		t.Errorf("nil input = %#v, want non-nil empty slice", out)
	}
	got := normalizeAmenities([]string{"Wi-Fi", "Free Wi-Fi", " Fitness center ", "($)", "", "Pet-friendly"})
	want := []string{"Wifi", "Free Wifi", "Gym", "Pet-friendly"}
	if len(got) != len(want) {
		t.Fatalf("normalized = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("normalized[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSerpapiMissingKeyFailsWithoutRequest(t *testing.T) {
	var hits int32
	c := testSerpapiClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	})
	c.apiKey = ""

	_, err := c.SearchPlaces(context.Background(), "Paris")
	if !errors.Is(err, ErrProviderFailed) {
		t.Fatalf("err = %v, want ErrProviderFailed", err)
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Errorf("server hits = %d, want 0 (no key → no request)", n)
	}
}

func TestSerpAPIErrorPayloadDegrades(t *testing.T) {
	c := testSerpapiClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"Google hasn't returned any results for this query."}`))
	})

	_, err := c.SearchPlaces(context.Background(), "nowhere")
	if !errors.Is(err, ErrProviderFailed) {
		t.Fatalf("err = %v, want wrapped ErrProviderFailed", err)
	}
	if !strings.Contains(err.Error(), "hasn't returned any results") {
		t.Errorf("err = %v, want the upstream message preserved", err)
	}
}

func TestSerpAPIRejectsNonSuccessStatus(t *testing.T) {
	c := testSerpapiClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"search_metadata":{"status":"Error"}}`))
	})

	_, err := c.SearchHotels(context.Background(), "Paris")
	if !errors.Is(err, ErrProviderFailed) {
		t.Fatalf("err = %v, want wrapped ErrProviderFailed", err)
	}
}

func TestSerpAPIRetriesOnceOn5xx(t *testing.T) {
	var hits int32
	c := testSerpapiClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(mapsFixture))
	})

	got, err := c.SearchPlaces(context.Background(), "Paris attractions")
	if err != nil {
		t.Fatalf("SearchPlaces after 5xx: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("results = %d, want 2 (retry succeeded)", len(got))
	}
	if n := atomic.LoadInt32(&hits); n != 2 {
		t.Errorf("hits = %d, want 2 (one retry)", n)
	}
}

func TestSerpAPINoRetryOn4xx(t *testing.T) {
	var hits int32
	c := testSerpapiClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
	})

	_, err := c.SearchPlaces(context.Background(), "Paris")
	if !errors.Is(err, ErrProviderFailed) {
		t.Fatalf("err = %v, want wrapped ErrProviderFailed", err)
	}
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("hits = %d, want 1 (4xx must not retry)", n)
	}
}
