package users

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the data-access boundary for users.
type Repository interface {
	GetByID(ctx context.Context, id int64) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
	Create(ctx context.Context, name, email, phone, passwordHash string) (*User, error)
	Update(ctx context.Context, id int64, u UpdateRequest) error
	// GetOrCreateByFirebaseUID creates or updates the Neon user row for a
	// verified Firebase ID token and returns the local user id. It is the
	// persistence half of Firebase Auth middleware.
	GetOrCreateByFirebaseUID(ctx context.Context, uid, email, name string) (int64, error)
	// GetRoleByFirebaseUID resolves the RBAC role ('user' | 'admin') for
	// AdminRequired — the source of truth behind the optional Firebase
	// custom claim (revocation takes effect on the next request).
	GetRoleByFirebaseUID(ctx context.Context, uid string) (string, error)
}

// PostgresRepository reads/writes Neon PostgreSQL.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

var _ Repository = (*PostgresRepository)(nil)

const userColumns = `id, COALESCE(firebase_uid,''), name, email, role, COALESCE(phone,''), COALESCE(avatar_url,''), COALESCE(bio,''), password_hash, created_at`

func (r *PostgresRepository) GetByID(ctx context.Context, id int64) (*User, error) {
	var u User
	err := r.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, id,
	).Scan(&u.ID, &u.FirebaseUID, &u.Name, &u.Email, &u.Role, &u.Phone, &u.AvatarURL, &u.Bio, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *PostgresRepository) GetByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	err := r.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE email = $1`, email,
	).Scan(&u.ID, &u.FirebaseUID, &u.Name, &u.Email, &u.Role, &u.Phone, &u.AvatarURL, &u.Bio, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *PostgresRepository) Create(ctx context.Context, name, email, phone, passwordHash string) (*User, error) {
	var u User
	// One sequence value feeds both id and the 'legacy:<id>' placeholder
	// (firebase_uid is UNIQUE NOT NULL, but password-era accounts have no
	// Firebase identity yet).
	err := r.pool.QueryRow(ctx, `
		WITH seq AS (SELECT nextval(pg_get_serial_sequence('users','id')) AS id)
		INSERT INTO users (id, firebase_uid, name, email, phone, password_hash)
		SELECT id, 'legacy:' || id::text, $1, $2, NULLIF($3,''), $4 FROM seq
		RETURNING `+userColumns,
		name, email, phone, passwordHash,
	).Scan(&u.ID, &u.FirebaseUID, &u.Name, &u.Email, &u.Role, &u.Phone, &u.AvatarURL, &u.Bio, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetOrCreateByFirebaseUID implements the Firebase Auth sync:
//
//  1. Row already linked to uid → refresh name/email best-effort.
//  2. Same email exists on a 'legacy:' password-era row → claim it (keeps
//     the user's id, favorites and bookings).
//  3. Otherwise insert a brand-new Neon user.
//
// Returns the local users.id that handlers read from the Gin context.
func (r *PostgresRepository) GetOrCreateByFirebaseUID(ctx context.Context, uid, email, name string) (int64, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return 0, errors.New("firebase uid is empty")
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		// Phone-only Firebase accounts have no email; keep email UNIQUE.
		email = uid + "@firebase.local"
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = strings.SplitN(email, "@", 2)[0]
	}

	// 1. Already linked — refresh profile fields best-effort.
	var id int64
	err := r.pool.QueryRow(ctx, `
		UPDATE users
		SET name  = CASE WHEN $3 <> '' THEN $3 ELSE name END,
		    email = CASE WHEN users.email IS DISTINCT FROM $2 THEN $2 ELSE users.email END
		WHERE firebase_uid = $1
		RETURNING id`, uid, email, name).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Not linked yet — fall through to claim/create.
	case isUniqueViolation(err):
		// Target email already belongs to another row — refresh name only.
		if err2 := r.pool.QueryRow(ctx, `
			UPDATE users SET name = CASE WHEN $2 <> '' THEN $2 ELSE name END
			WHERE firebase_uid = $1 RETURNING id`, uid, name).Scan(&id); err2 != nil {
			return 0, err2
		}
		return id, nil
	default:
		return 0, err
	}

	// 2. Claim a legacy password-era row that matches this email.
	err = r.pool.QueryRow(ctx, `
		UPDATE users
		SET firebase_uid = $1,
		    name = CASE WHEN users.name = '' THEN $3 ELSE users.name END
		WHERE email = $2 AND (firebase_uid LIKE 'legacy:%' OR firebase_uid = '')
		RETURNING id`, uid, email, name).Scan(&id)
	switch {
	case err == nil:
		return id, nil
	case errors.Is(err, pgx.ErrNoRows):
		// No legacy row — insert below.
	default:
		return 0, err
	}

	// 3. Brand-new Firebase user.
	err = r.pool.QueryRow(ctx, `
		INSERT INTO users (firebase_uid, name, email)
		VALUES ($1, $2, $3)
		ON CONFLICT (firebase_uid) DO UPDATE SET firebase_uid = EXCLUDED.firebase_uid
		RETURNING id`, uid, name, email).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("email %q is already linked to a different Firebase account", email)
		}
		return 0, err
	}
	return id, nil
}

// GetRoleByFirebaseUID implements the AdminRequired lookup: one indexed
// read of users.role for the verified UID from the request context.
func (r *PostgresRepository) GetRoleByFirebaseUID(ctx context.Context, uid string) (string, error) {
	var role string
	if err := r.pool.QueryRow(ctx,
		`SELECT role FROM users WHERE firebase_uid = $1`, uid,
	).Scan(&role); err != nil {
		return "", err
	}
	return role, nil
}

func (r *PostgresRepository) Update(ctx context.Context, id int64, u UpdateRequest) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE users SET
			name        = COALESCE($2, name),
			phone       = COALESCE($3, phone),
			avatar_url  = COALESCE($4, avatar_url),
			bio         = COALESCE($5, bio)
		WHERE id = $1`,
		id, u.Name, u.Phone, u.AvatarURL, u.Bio)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
