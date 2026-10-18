// Package firebase isolates Firebase Admin SDK initialization.
// Nothing outside this package imports firebase.google.com/go/v4 directly.
package firebase

import (
	"context"
	"fmt"
	"os"
	"strings"

	firebase "firebase.google.com/go/v4"
	"firebase.google.com/go/v4/auth"
	"google.golang.org/api/option"
)

// Credentials carries the two supported ways to supply a service account.
// Exactly one should be set; if both are set, raw JSON wins.
type Credentials struct {
	// ServiceAccountJSON is either the raw service-account JSON document
	// (starts with '{') or a path to one (FIREBASE_SERVICE_ACCOUNT_JSON).
	ServiceAccountJSON string
	// File is a path to a service-account JSON file (FIREBASE_CREDENTIALS_FILE).
	File string
}

// Client bundles the initialized Admin SDK app and its auth client.
type Client struct {
	App  *firebase.App
	Auth *auth.Client
}

// Init builds firebase.App + auth.Client from the given credentials.
// It fails fast with an actionable error so the server never starts half-configured.
func Init(ctx context.Context, creds Credentials) (*Client, error) {
	opts, err := credentialOptions(creds)
	if err != nil {
		return nil, err
	}

	app, err := firebase.NewApp(ctx, nil, opts...)
	if err != nil {
		// Avoid logging credential parser errors, which can contain key material.
		return nil, fmt.Errorf("firebase: initialize app: credentials could not be validated (details redacted)")
	}

	authClient, err := app.Auth(ctx)
	if err != nil {
		// Some Google credential errors include the parsed private key in their
		// message. Do not propagate that error into application logs.
		return nil, fmt.Errorf("firebase: initialize auth client: credentials could not be validated (details redacted)")
	}

	return &Client{App: app, Auth: authClient}, nil
}

// Identity is the verified-token payload the rest of the backend needs.
// Defined here so no package outside pkg/firebase ever touches SDK types.
type Identity struct {
	UID           string
	Email         string
	EmailVerified bool
	Name          string
	Picture       string
}

// VerifyIDToken verifies a Firebase ID token and returns its identity claims.
func (c *Client) VerifyIDToken(ctx context.Context, idToken string) (*Identity, error) {
	token, err := c.Auth.VerifyIDToken(ctx, idToken)
	if err != nil {
		return nil, err
	}
	id := &Identity{UID: token.UID}
	if v, ok := token.Claims["email"].(string); ok {
		id.Email = v
	}
	if v, ok := token.Claims["name"].(string); ok {
		id.Name = v
	}
	if v, ok := token.Claims["picture"].(string); ok {
		id.Picture = v
	}
	if v, ok := token.Claims["email_verified"].(bool); ok {
		id.EmailVerified = v
	}
	return id, nil
}

// credentialOptions resolves Credentials into google.golang.org/api ClientOptions.
func credentialOptions(creds Credentials) ([]option.ClientOption, error) {
	raw := strings.TrimSpace(creds.ServiceAccountJSON)
	if raw != "" {
		// Accept raw JSON inline, or a path for operational convenience.
		if strings.HasPrefix(raw, "{") {
			return []option.ClientOption{option.WithCredentialsJSON([]byte(raw))}, nil
		}
		return fileOption(raw, "FIREBASE_SERVICE_ACCOUNT_JSON")
	}

	file := strings.TrimSpace(creds.File)
	if file != "" {
		return fileOption(file, "FIREBASE_CREDENTIALS_FILE")
	}

	return nil, fmt.Errorf(
		"firebase: no credentials configured — set FIREBASE_SERVICE_ACCOUNT_JSON (raw JSON) or FIREBASE_CREDENTIALS_FILE (path) in .env",
	)
}

func fileOption(path, envName string) ([]option.ClientOption, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("firebase: %s must be a readable path to a service-account JSON file", envName)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("firebase: %s must point to a service-account JSON file, not a directory", envName)
	}
	return []option.ClientOption{option.WithCredentialsFile(path)}, nil
}
