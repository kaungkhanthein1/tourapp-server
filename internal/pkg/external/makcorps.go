package external

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// USDToMMK converts Makcorps USD prices into the app's MMK pricing display.
// Adjust occasionally to match market rate.
const USDToMMK = 2100

// HotelResult is the provider-neutral hotel shape cached into Neon.
type HotelResult struct {
	Name     string
	Place    string
	Country  string
	ImageURL string
	Rating   float64
	PriceMMK int64
}

// HotelSearcher is the adapter interface consumed by the hotels Service.
type HotelSearcher interface {
	SearchHotels(ctx context.Context, place string) ([]HotelResult, error)
}

// MakcorpsClient calls the Makcorps free hotel search API.
type MakcorpsClient struct {
	apiKey string
	http   *http.Client
}

func NewMakcorpsClient(apiKey string) *MakcorpsClient {
	return &MakcorpsClient{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 8 * time.Second},
	}
}

var _ HotelSearcher = (*MakcorpsClient)(nil)

const makcorpsURL = "https://api.makcorps.com/api/search/"

// SearchHotels queries Makcorps for hotels in the given place.
// The upstream payload is not fully documented, so parsing is lenient:
// we accept either {"hotels":[...]} or a bare array of objects with
// name/price fields.
func (c *MakcorpsClient) SearchHotels(ctx context.Context, place string) ([]HotelResult, error) {
	if c.apiKey == "" {
		return nil, ErrNotConfigured
	}

	u := fmt.Sprintf("%s%s?api_key=%s", makcorpsURL, url.PathEscape(place), url.QueryEscape(c.apiKey))
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

	var raw any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrProviderFailed, err)
	}

	items := extractHotelItems(raw)
	out := make([]HotelResult, 0, len(items))
	for _, it := range items {
		name := firstString(it, "HotelName", "hotel_name", "name", "Name")
		if name == "" {
			continue
		}
		priceUSD := firstFloat(it, "Price", "price", "avg_price", "AvgPrice")
		out = append(out, HotelResult{
			Name:     name,
			Place:    place,
			Country:  firstString(it, "country", "Country"),
			ImageURL: firstString(it, "image_url", "ImageURL", "photo", "img"),
			Rating:   firstFloat(it, "Rating", "rating", "stars"),
			PriceMMK: int64(priceUSD * USDToMMK),
		})
	}
	return out, nil
}

// extractHotelItems pulls the hotel object slice out of a lenient payload.
func extractHotelItems(raw any) []map[string]any {
	obj, ok := raw.(map[string]any)
	if !ok {
		if arr, ok := raw.([]any); ok {
			return toMaps(arr)
		}
		return nil
	}
	for _, key := range []string{"hotels", "Hotels", "data", "results"} {
		if arr, ok := obj[key].([]any); ok {
			return toMaps(arr)
		}
	}
	return nil
}

func toMaps(arr []any) []map[string]any {
	out := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func firstFloat(m map[string]any, keys ...string) float64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return v
		case string:
			var f float64
			if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
				return f
			}
		}
	}
	return 0
}
