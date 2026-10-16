package users

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"travel-app-backend/internal/pkg/auth"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

// Domain errors.
var (
	ErrNotFound       = errors.New("user not found")
	ErrEmailTaken     = errors.New("email already registered")
	ErrBadCredentials = errors.New("invalid email or password")
	ErrInvalidAvatar  = errors.New("avatar_url must be an http(s) URL")
)

// Service holds user auth + profile logic behind the Repository interface.
type Service struct {
	repo Repository
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Register creates an account and returns a signed JWT.
func (s *Service) Register(ctx context.Context, req RegisterRequest) (*AuthResponse, error) {
	if _, err := s.repo.GetByEmail(ctx, strings.ToLower(strings.TrimSpace(req.Email))); err == nil {
		return nil, ErrEmailTaken
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	u, err := s.repo.Create(ctx,
		strings.TrimSpace(req.Name),
		strings.ToLower(strings.TrimSpace(req.Email)),
		strings.TrimSpace(req.Phone),
		string(hash))
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrEmailTaken
		}
		return nil, err
	}

	token, err := auth.GenerateToken(u.ID, u.Email)
	if err != nil {
		return nil, err
	}
	return &AuthResponse{Token: token, User: *u}, nil
}

// Login verifies credentials and returns a signed JWT.
func (s *Service) Login(ctx context.Context, req LoginRequest) (*AuthResponse, error) {
	u, err := s.repo.GetByEmail(ctx, strings.ToLower(strings.TrimSpace(req.Email)))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrBadCredentials
		}
		return nil, err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(req.Password)) != nil {
		return nil, ErrBadCredentials
	}

	token, err := auth.GenerateToken(u.ID, u.Email)
	if err != nil {
		return nil, err
	}
	return &AuthResponse{Token: token, User: *u}, nil
}

func (s *Service) GetByID(ctx context.Context, id int64) (*User, error) {
	u, err := s.repo.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get user %d: %w", id, err)
	}
	return u, nil
}

// SyncFirebase upserts a verified Firebase identity into Neon and returns
// the local profile. The client calls POST /users/sync after email
// verification; FirebaseAuthMiddleware already performs the same sync on
// every protected request, so this endpoint is idempotent.
func (s *Service) SyncFirebase(ctx context.Context, uid, email, name string) (*User, error) {
	id, err := s.repo.GetOrCreateByFirebaseUID(ctx, uid, email, name)
	if err != nil {
		return nil, fmt.Errorf("sync firebase user: %w", err)
	}
	return s.GetByID(ctx, id)
}

func (s *Service) Update(ctx context.Context, id int64, req UpdateRequest) (*User, error) {
	// The backend never handles file bytes — only the final public URL string
	// (uploaded client-side to Supabase Storage). Validate before persisting.
	if req.AvatarURL != nil {
		u := strings.TrimSpace(*req.AvatarURL)
		req.AvatarURL = &u
		if u != "" {
			parsed, err := url.Parse(u)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
				return nil, ErrInvalidAvatar
			}
			if len(u) > 2048 {
				return nil, ErrInvalidAvatar
			}
		}
	}
	if err := s.repo.Update(ctx, id, req); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.GetByID(ctx, id)
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLSTATE 23505")
}
