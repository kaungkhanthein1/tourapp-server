package external

import (
	"context"
	"errors"
	"net/http"
	"strconv"
)

// ErrProviderFailed wraps upstream errors (timeouts, connection resets,
// 429, 5xx) so seed paths can degrade to database data instead of
// failing the run.
var ErrProviderFailed = errors.New("external provider request failed")

// PlaceResult is the provider-neutral place shape cached into Neon.
// Lat/Lon travel end-to-end so cached rows carry their coordinates.
type PlaceResult struct {
	Name        string
	Country     string
	Region      string
	Category    string
	ImageURL    string
	Rating      float64
	Description string
	Lat         float64
	Lon         float64
	Gallery     []string
}

// PlaceSearcher is the adapter interface consumed by the places Service
// at seed time only — the runtime never fetches places (preset-data
// policy); bootstrap walks labeled queries and caches them into Neon.
type PlaceSearcher interface {
	SearchPlaces(ctx context.Context, query string) ([]PlaceResult, error)
}

// statusError is a non-200 response. It unwraps to ErrProviderFailed so
// services degrade to database data, while callers can detect 429 vs 5xx
// for retry decisions.
type statusError struct {
	code int
}

func (e *statusError) Error() string {
	if e.code == http.StatusTooManyRequests {
		return "external provider request failed: rate limited (http 429)"
	}
	return "external provider request failed: status " + strconv.Itoa(e.code)
}

func (e *statusError) Unwrap() error { return ErrProviderFailed }

// retryable reports whether an error class deserves the second attempt:
// transport failures, rate limits and server errors. Other HTTP
// statuses will not change on an immediate retry.
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code == http.StatusTooManyRequests || se.code >= 500
	}
	return true
}
