package config

import (
	"fmt"
	"os"
	"strings"
)

// Config holds validated runtime configuration.
type Config struct {
	DatabaseURL       string
	Port              string
	GinMode           string
	JWTSecret         string
	CORSAllowedOrigins []string
	GooglePlacesAPIKey string
	MakcorpsAPIKey     string
	MockDataDir        string

	// Firebase service-account credentials. At least one must be set;
	// validated in Load so the server never starts half-configured.
	FirebaseServiceAccountJSON string // raw JSON (or a path) from FIREBASE_SERVICE_ACCOUNT_JSON
	FirebaseCredentialsFile    string // path from FIREBASE_CREDENTIALS_FILE
}

// Load reads the environment and fails fast on missing required values.
func Load() (*Config, error) {
	cfg := &Config{
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		Port:               envOr("PORT", "8080"),
		GinMode:            envOr("GIN_MODE", "release"),
		JWTSecret:          os.Getenv("JWT_SECRET"),
		GooglePlacesAPIKey: os.Getenv("GOOGLE_PLACES_API_KEY"),
		MakcorpsAPIKey:     os.Getenv("MAKCORPS_API_KEY"),
		MockDataDir:        envOr("MOCK_DATA_DIR", "mock_data"),

		FirebaseServiceAccountJSON: os.Getenv("FIREBASE_SERVICE_ACCOUNT_JSON"),
		FirebaseCredentialsFile:    os.Getenv("FIREBASE_CREDENTIALS_FILE"),
	}

	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("config: DATABASE_URL is required")
	}
	if cfg.FirebaseServiceAccountJSON == "" && cfg.FirebaseCredentialsFile == "" {
		return nil, fmt.Errorf(
			"config: Firebase credentials are required — set FIREBASE_SERVICE_ACCOUNT_JSON (raw service-account JSON) or FIREBASE_CREDENTIALS_FILE (path to the JSON file) in .env",
		)
	}
	if cfg.JWTSecret != "" && len(cfg.JWTSecret) < 16 {
		return nil, fmt.Errorf("config: JWT_SECRET must be at least 16 characters")
	}

	origins := os.Getenv("CORS_ALLOWED_ORIGINS")
	if origins == "" {
		origins = "*"
	}
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.CORSAllowedOrigins = append(cfg.CORSAllowedOrigins, o)
		}
	}

	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
