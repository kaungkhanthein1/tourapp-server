package external

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ErrNotConfigured is returned when an adapter has no API key; services
// treat it as "DB-only mode" and continue silently.
var ErrNotConfigured = errors.New("external provider not configured")

// ErrProviderFailed wraps upstream errors (timeouts, 5xx) so services can
// fall back to returning an empty/DB-only result.
var ErrProviderFailed = errors.New("external provider request failed")

// PlaceResult is the provider-neutral place shape cached into Neon.
type PlaceResult struct {
	Name        string
	Country     string
	Region      string
	Category    string
	ImageURL    string
	Rating      float64
	Description string
}

// PlaceSearcher is the adapter interface consumed by the places Service.
type PlaceSearcher interface {
	SearchPlaces(ctx context.Context, query string) ([]PlaceResult, error)
}

// GooglePlacesClient calls the Google Places Text Search HTTP API.
type GooglePlacesClient struct {
	apiKey string
	http   *http.Client
}

func NewGooglePlacesClient(apiKey string) *GooglePlacesClient {
	return &GooglePlacesClient{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 8 * time.Second},
	}
}

var _ PlaceSearcher = (*GooglePlacesClient)(nil)

const googlePlacesURL = "https://maps.googleapis.com/maps/api/place/textsearch/json"

type googlePlacesResponse struct {
	Results []struct {
		Name            string  `json:"name"`
		FormattedAddress string  `json:"formatted_address"`
		Rating          float64 `json:"rating"`
		Photos          []struct {
			PhotoReference string `json:"photo_reference"`
		} `json:"photos"`
		Types []string `json:"types"`
	} `json:"results"`
	Status string `json:"status"`
}

// SearchPlaces queries Google Places Text Search for the given query.
func (c *GooglePlacesClient) SearchPlaces(ctx context.Context, query string) ([]PlaceResult, error) {
	if c.apiKey == "" {
		return nil, ErrNotConfigured
	}

	u := fmt.Sprintf("%s?query=%s&key=%s", googlePlacesURL, url.QueryEscape(query), url.QueryEscape(c.apiKey))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderFailed, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrProviderFailed, resp.StatusCode)
	}

	var parsed googlePlacesResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrProviderFailed, err)
	}
	if parsed.Status != "OK" && parsed.Status != "ZERO_RESULTS" {
		return nil, fmt.Errorf("%w: google status %s", ErrProviderFailed, parsed.Status)
	}

	out := make([]PlaceResult, 0, len(parsed.Results))
	for _, r := range parsed.Results {
		image := ""
		if len(r.Photos) > 0 {
			image = fmt.Sprintf("https://maps.googleapis.com/maps/api/place/photo?maxwidth=800&photoreference=%s&key=%s",
				url.QueryEscape(r.Photos[0].PhotoReference), url.QueryEscape(c.apiKey))
		}
		country, region := splitAddress(r.FormattedAddress)
		out = append(out, PlaceResult{
			Name:        r.Name,
			Country:     country,
			Region:      region,
			Category:    firstOf(r.Types, "Cities"),
			ImageURL:    image,
			Rating:      r.Rating,
			Description: r.FormattedAddress,
		})
	}
	return out, nil
}

// splitAddress derives (country, region) from a formatted address tail.
func splitAddress(formatted string) (country, region string) {
	parts := splitTrim(formatted)
	if len(parts) == 0 {
		return "", ""
	}
	country = parts[len(parts)-1]
	if len(parts) > 1 {
		region = parts[len(parts)-2]
	}
	return country, region
}

func splitTrim(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if t := trimSpace(cur); t != "" {
				out = append(out, t)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if t := trimSpace(cur); t != "" {
		out = append(out, t)
	}
	return out
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && s[start] == ' ' {
		start++
	}
	for end > start && s[end-1] == ' ' {
		end--
	}
	return s[start:end]
}

func firstOf(values []string, fallback string) string {
	if len(values) > 0 && values[0] != "" {
		return titleCase(values[0])
	}
	return fallback
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 32
	}
	return string(b)
}
