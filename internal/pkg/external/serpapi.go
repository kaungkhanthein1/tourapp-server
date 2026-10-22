package external

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SerpAPIClient talks to SerpApi's google_maps and google_hotels
// engines. It exists only for the one-shot bootstrap seeder
// (cmd/bootstrap -serpapi): the running server serves hotels from Neon
// only (preset-data policy), so the paid key never serves live traffic.
// One plan credit (~250/month) is spent per query — the client paces
// request starts, retries once on 429/5xx, and reports every failure
// wrapped in ErrProviderFailed (with URLs redacted) so callers degrade
// to database data.
type SerpAPIClient struct {
	apiKey      string
	baseURL     string
	userAgent   string
	minInterval time.Duration
	http        *http.Client

	mu   sync.Mutex
	next time.Time
}

const (
	serpapiBaseURL = "https://serpapi.com/search.json"

	serpapiUserAgent = "TourApp/1.0 (contact@tourapp.dev)"

	// serpapiMinInterval spaces request starts across the client so the
	// seeder's parallel query fan-out cannot burst the plan's rate.
	serpapiMinInterval = 700 * time.Millisecond

	// usdToMMKRate converts google_hotels' USD nightly rate into the
	// app's MMK price column. It is an approximate, indicative figure —
	// the room description labels it as such.
	usdToMMKRate = 2100
)

var (
	_ PlaceSearcher = (*SerpAPIClient)(nil)
	_ HotelSearcher = (*SerpAPIClient)(nil)
)

// NewSerpAPIClient builds a client for the given plan key. An empty key
// is accepted at construction (wiring stays unconditional) and rejected
// on the first request.
func NewSerpAPIClient(apiKey string) *SerpAPIClient {
	return &SerpAPIClient{
		apiKey:      strings.TrimSpace(apiKey),
		baseURL:     serpapiBaseURL,
		userAgent:   serpapiUserAgent,
		minInterval: serpapiMinInterval,
		http:        &http.Client{Timeout: 15 * time.Second},
	}
}

// serpapiResponse is the union of the two engine payloads this client
// reads: google_maps fills local_results, google_hotels fills
// properties; error/search_metadata are common to both. Fields follow
// the live probe payloads (2026-10-21).
type serpapiResponse struct {
	Error          string `json:"error"`
	SearchMetadata struct {
		Status string `json:"status"`
	} `json:"search_metadata"`
	LocalResults []serpapiLocalResult `json:"local_results"`
	Properties   []serpapiProperty    `json:"properties"`
}

type serpapiLocalResult struct {
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	Address        string   `json:"address"`
	Country        string   `json:"country"`
	Type           string   `json:"type"`
	Types          []string `json:"types"`
	Rating         float64  `json:"rating"`
	Thumbnail      string   `json:"thumbnail"`
	GPSCoordinates struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"gps_coordinates"`
}

type serpapiProperty struct {
	Name          string  `json:"name"`
	OverallRating float64 `json:"overall_rating"`
	RatePerNight  struct {
		ExtractedLowest float64 `json:"extracted_lowest"`
	} `json:"rate_per_night"`
	Amenities []string `json:"amenities"`
	Images    []struct {
		OriginalImage string `json:"original_image"`
	} `json:"images"`
	GPSCoordinates struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"gps_coordinates"`
}

// SearchPlaces runs a google_maps text search and maps each local
// result into the provider-neutral place shape (country/region parsed
// from the address, chip from Google's type taxonomy, photo scaled up).
func (c *SerpAPIClient) SearchPlaces(ctx context.Context, query string) ([]PlaceResult, error) {
	params := url.Values{}
	params.Set("engine", "google_maps")
	params.Set("type", "search")
	params.Set("q", query)
	params.Set("hl", "en")

	resp, err := c.do(ctx, params)
	if err != nil {
		return nil, err
	}
	out := make([]PlaceResult, 0, len(resp.LocalResults))
	for _, r := range resp.LocalResults {
		if res, ok := mapSerpapiPlace(r); ok {
			out = append(out, res)
		}
	}
	return out, nil
}

// SearchHotels runs a google_hotels search for the given place text and
// maps each property into the provider-neutral hotel shape. The query
// may carry a country after a comma ("Paris, France"): the anchor
// becomes each row's Place (so the place = $1 re-read matches existing
// rows) and the country fills the Country column. google_hotels prices
// are property-level, so every hotel carries one Standard Room at the
// converted nightly rate.
func (c *SerpAPIClient) SearchHotels(ctx context.Context, place string) ([]HotelResult, error) {
	checkIn, checkOut := googleHotelDates(time.Now())
	params := url.Values{}
	params.Set("engine", "google_hotels")
	params.Set("q", place)
	params.Set("gl", "us")
	params.Set("hl", "en")
	params.Set("currency", "USD")
	params.Set("check_in_date", checkIn)
	params.Set("check_out_date", checkOut)
	params.Set("adults", "2")
	params.Set("children", "0")

	resp, err := c.do(ctx, params)
	if err != nil {
		return nil, err
	}
	anchor, country := splitAnchorQuery(place)
	out := make([]HotelResult, 0, len(resp.Properties))
	for _, p := range resp.Properties {
		if res, ok := mapSerpapiHotel(p, anchor, country); ok {
			out = append(out, res)
		}
	}
	return out, nil
}

// do sends one engine request (pacing the start, retrying once on the
// transient classes retryable reports) and validates the envelope.
func (c *SerpAPIClient) do(ctx context.Context, params url.Values) (*serpapiResponse, error) {
	if c.apiKey == "" {
		return nil, fmt.Errorf("%w: SERPAPI_KEY missing (add it to .env for -serpapi)", ErrProviderFailed)
	}
	params.Set("api_key", c.apiKey)
	rawURL := c.baseURL + "?" + params.Encode()

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.pace(ctx); err != nil {
			return nil, err
		}
		resp, err := c.once(ctx, rawURL)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if attempt == 0 && retryable(err) {
			continue
		}
		break
	}
	return nil, lastErr
}

// once performs a single HTTP round-trip and decodes the envelope.
// Upstream errors arrive either as a non-200 status or as the payload's
// error field (HTTP 200 with no results); both degrade as
// ErrProviderFailed.
func (c *SerpAPIClient) once(ctx context.Context, rawURL string) (*serpapiResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderFailed, err)
	}
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderFailed, redactRequestError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, &statusError{code: resp.StatusCode}
	}
	var out serpapiResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrProviderFailed, err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("%w: serpapi: %s", ErrProviderFailed, out.Error)
	}
	if st := out.SearchMetadata.Status; st != "" && st != "Success" {
		return nil, fmt.Errorf("%w: serpapi status %s", ErrProviderFailed, st)
	}
	return &out, nil
}

// redactRequestError strips the query string (which carries api_key)
// from a transport error's URL — request failures must never put the
// plan key into logs.
func redactRequestError(err error) error {
	if ue, ok := err.(*url.Error); ok && ue.URL != "" {
		copied := *ue
		if u, perr := url.Parse(ue.URL); perr == nil {
			u.RawQuery = ""
			copied.URL = u.String()
		}
		return &copied
	}
	return err
}

// pace spaces request starts across the client (see serpapiMinInterval)
// and honors cancellation while a caller waits for its slot. A zero
// interval (tests) is a no-op.
func (c *SerpAPIClient) pace(ctx context.Context) error {
	if c.minInterval <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := time.Until(c.next); wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	c.next = time.Now().Add(c.minInterval)
	return nil
}

// googleHotelDates picks the stay window sent to google_hotels: rates
// are date-sensitive, so a rolling future window keeps seeded prices
// meaningful whenever the seeder runs.
func googleHotelDates(now time.Time) (checkIn, checkOut string) {
	in := now.AddDate(0, 0, 14)
	return in.Format("2006-01-02"), in.AddDate(0, 0, 2).Format("2006-01-02")
}

// mapSerpapiPlace maps one google_maps local result. Rows without a
// title, or whose types identify a service/agency rather than a
// destination (tour operators, photo services — seen live in the
// probe), are skipped; the feed's prose bar drops the rest of the junk.
func mapSerpapiPlace(r serpapiLocalResult) (PlaceResult, bool) {
	name := strings.TrimSpace(r.Title)
	if name == "" {
		return PlaceResult{}, false
	}
	category := classifyPlace(r.Type, r.Types)
	if category == "" {
		return PlaceResult{}, false
	}
	region, country := serpapiCountryRegion(r.Address, r.Country)
	return PlaceResult{
		Name:        name,
		Country:     country,
		Region:      region,
		Category:    category,
		ImageURL:    scaleGooglePhoto(r.Thumbnail),
		Rating:      r.Rating,
		Description: strings.TrimSpace(r.Description),
		Lat:         r.GPSCoordinates.Latitude,
		Lon:         r.GPSCoordinates.Longitude,
		// google_maps returns a single thumbnail per result.
		Gallery: nil,
	}, true
}

// classifyPlace maps Google's place taxonomy onto the app's six Explore
// chips (categories.json: Cities, Architecture, Foods, Parks, Hills,
// Beaches). Type entries are matched as whole words in chip priority
// order — Beaches, Parks, Hills, Cities, Foods — and anything unmatched
// falls through to Architecture, the feed's sightseeing catch-all
// (monuments, museums, temples, castles all live there). Cities matches
// whole entries only, so a "City park" stays Parks. An empty category
// means "not a destination": service and agency listings are never
// cards.
func classifyPlace(kind string, types []string) string {
	entries := make([]string, 0, len(types)+1)
	if kind != "" {
		entries = append(entries, kind)
	}
	entries = append(entries, types...)

	words := make(map[string]bool, 16)
	for _, e := range entries {
		for _, w := range strings.Fields(strings.ToLower(e)) {
			words[w] = true
		}
	}
	has := func(tokens ...string) bool {
		for _, t := range tokens {
			if words[t] {
				return true
			}
		}
		return false
	}

	if has("agency", "service", "escort") {
		return ""
	}
	switch {
	case has("beach", "coast", "seaside", "lagoon", "reef"):
		return "Beaches"
	case has("park", "garden", "playground", "forest", "jungle", "lake"):
		return "Parks"
	case has("volcano", "mountain", "hill", "hills", "summit", "valley",
		"ridge", "canyon", "cave", "waterfall", "desert", "dune", "glacier"):
		return "Hills"
	case entryMatch(entries, "city", "town", "village"):
		return "Cities"
	case has("restaurant", "cafe", "coffee", "food", "bakery", "bar",
		"pub", "market", "eatery", "deli", "pizza", "noodle"):
		return "Foods"
	default:
		return "Architecture"
	}
}

// entryMatch reports whether any entry equals one of the tokens
// (whole-entry, case-insensitive) — the Cities rule: only a literal
// city/town/village entry maps to the chip.
func entryMatch(entries []string, tokens ...string) bool {
	for _, e := range entries {
		for _, t := range tokens {
			if strings.EqualFold(strings.TrimSpace(e), t) {
				return true
			}
		}
	}
	return false
}

// serpapiCountryRegion splits Google's address into (region, country).
// The country field is authoritative (otherwise the trailing address
// segment is taken as the country); the region is the locality line
// just before it with its leading postcode stripped
// ("Pl. X, 75008 Paris, France" → "Paris", "France").
func serpapiCountryRegion(address, country string) (string, string) {
	parts := splitTrim(strings.TrimSpace(address), ",")
	ctr := strings.TrimSpace(country)
	if len(parts) > 0 {
		last := parts[len(parts)-1]
		switch {
		case ctr == "":
			ctr = last
			parts = parts[:len(parts)-1]
		case strings.EqualFold(last, ctr):
			parts = parts[:len(parts)-1]
		}
	}
	if len(parts) == 0 {
		return "", ctr
	}
	region := stripLeadingPostcode(parts[len(parts)-1])
	if strings.EqualFold(region, ctr) {
		return "", ctr
	}
	return region, ctr
}

// stripLeadingPostcode removes a leading postcode from a locality line
// ("75008 Paris" → "Paris"); lines without digits pass through.
func stripLeadingPostcode(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return strings.TrimSpace(s[i:])
}

// splitAnchorQuery parses a seeder hotel query ("Paris, France") into
// the anchor echoed as each row's Place and the trailing country.
func splitAnchorQuery(q string) (anchor, country string) {
	parts := splitTrim(q, ",")
	if len(parts) == 0 {
		return strings.TrimSpace(q), ""
	}
	anchor = parts[0]
	if len(parts) > 1 {
		country = strings.Join(parts[1:], ", ")
	}
	return anchor, country
}

// splitTrim splits on sep and keeps non-blank trimmed segments.
func splitTrim(s, sep string) []string {
	if s == "" {
		return nil
	}
	raw := strings.Split(s, sep)
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// scaleGooglePhoto upgrades Google's size-suffixed image URLs to the
// large (=s1600) render. Verified against live URLs (2026-10-21): the
// rewrite loads for =s10000, =w1000-h1000-c-n, =80 and =medium, while
// a bare URL rejects any suffix (403), =1 rejects it (400) and an
// =AQAB… token URL serves a broken stub — so only recognized size
// tokens are rewritten and everything else ships untouched.
func scaleGooglePhoto(raw string) string {
	if raw == "" {
		return raw
	}
	i := strings.LastIndexByte(raw, '=')
	if i < 0 {
		return raw // bare URL — never append
	}
	if !googleSizeSuffix(raw[i+1:]) {
		return raw
	}
	return raw[:i] + "=s1600"
}

// googleSizeSuffix reports whether an image URL suffix is a known size
// token: a single s/w/h size (s10000), a w×h dimension pair
// (w1000-h1000-c-n), or the small "80"/"medium" variants. Auth tokens
// (=AQAB…), bare "1" and compound tokens (s287-w287-h192…) are not
// rewritten — they were not shown to accept a resize.
func googleSizeSuffix(suffix string) bool {
	switch suffix {
	case "80", "medium":
		return true
	}
	if len(suffix) < 2 {
		return false
	}
	if suffix[0] == 's' || suffix[0] == 'w' || suffix[0] == 'h' {
		if allDigits(suffix[1:]) {
			return true
		}
	}
	if suffix[0] == 'w' {
		if j := strings.IndexByte(suffix, '-'); j > 1 && allDigits(suffix[1:j]) {
			return true
		}
	}
	return false
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// mapSerpapiHotel maps one google_hotels property. The hero photo is
// the first scaled image and the remaining photos travel in the gallery,
// and the extracted USD rate becomes one clearly-labeled MMK room.
func mapSerpapiHotel(p serpapiProperty, anchor, country string) (HotelResult, bool) {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return HotelResult{}, false
	}

	var hero string
	var gallery []string
	seen := make(map[string]bool, 4)
	for _, img := range p.Images {
		u := scaleGooglePhoto(strings.TrimSpace(img.OriginalImage))
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		if hero == "" {
			hero = u
			continue
		}
		gallery = append(gallery, u)
	}

	return HotelResult{
		Name:       name,
		Place:      anchor,
		Country:    country,
		Region:     anchor,
		ImageURL:   hero,
		Rating:     p.OverallRating,
		Rooms:      []RoomResult{googleHotelRoom(p.RatePerNight.ExtractedLowest)},
		Facilities: normalizeAmenities(p.Amenities),
		Gallery:    gallery,
	}, true
}

// googleHotelRoom builds the single bookable room for a google_hotels
// property: the extracted nightly rate in USD, converted to the app's
// MMK column at the approximate rate, with a description that says so;
// without a rate the shared example-room price applies. google_hotels
// has no per-room inventory, so one Standard Room is the honest shape.
func googleHotelRoom(usdPerNight float64) RoomResult {
	rm := RoomResult{
		Name:        "Standard Room",
		PriceMMK:    exampleRoomPriceMMK,
		Capacity:    2,
		Description: exampleRoomDescription,
	}
	if usdPerNight > 0 {
		mmk := int64(math.Round(usdPerNight * usdToMMKRate))
		rm.PriceMMK = mmk
		rm.Description = fmt.Sprintf(
			"Indicative rate: US$%s per night (≈ MMK %d at 1 USD = %d MMK) — confirm pricing with the property before booking.",
			strconv.FormatFloat(usdPerNight, 'f', -1, 64), mmk, int64(usdToMMKRate))
	}
	return rm
}

// normalizeAmenities maps Google's amenity strings onto the hotel
// card's icon vocabulary (FACILITY_ICONS: breakfast, swimming pool,
// spa, free wifi, wifi, parking, room service, gym, bar), strips the
// "($)" fee marker, dedupes and caps the list so the card's wrap row
// stays short. Unmapped amenities keep their text and fall back to the
// generic checkmark icon on the client.
func normalizeAmenities(in []string) []string {
	out := make([]string, 0, 8)
	if in == nil {
		return out
	}
	seen := make(map[string]bool, len(in))
	for _, a := range in {
		s := strings.ReplaceAll(strings.TrimSpace(a), " ($)", "")
		s = strings.ReplaceAll(s, "Wi-Fi", "Wifi")
		switch s {
		case "($)", "":
			s = "" // a bare fee marker carries no amenity
		case "Fitness center":
			s = "Gym"
		case "Indoor pool", "Outdoor pool":
			s = "Swimming pool"
		}
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
		if len(out) == 8 {
			break
		}
	}
	return out
}
