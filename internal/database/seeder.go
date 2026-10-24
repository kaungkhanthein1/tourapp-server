package database

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS users (
	id            BIGSERIAL PRIMARY KEY,
	firebase_uid  VARCHAR(128) NOT NULL UNIQUE,
	name          TEXT NOT NULL,
	email         TEXT NOT NULL UNIQUE,
	phone         TEXT,
	avatar_url    TEXT,
	bio           TEXT,
	password_hash TEXT NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS categories (
	id         BIGSERIAL PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	icon       TEXT,
	sort_order INT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS places (
	id           BIGSERIAL PRIMARY KEY,
	name         TEXT NOT NULL,
	country      TEXT NOT NULL,
	region       TEXT,
	category     TEXT NOT NULL,
	image_url    TEXT NOT NULL,
	gallery_urls TEXT[] NOT NULL DEFAULT '{}',
	rating       REAL NOT NULL DEFAULT 0,
	is_popular   BOOLEAN NOT NULL DEFAULT false,
	is_trending  BOOLEAN NOT NULL DEFAULT false,
	description  TEXT,
	source       TEXT NOT NULL DEFAULT 'seed',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_places_category ON places (category);
CREATE INDEX IF NOT EXISTS idx_places_popular  ON places (is_popular) WHERE is_popular;
CREATE INDEX IF NOT EXISTS idx_places_trending ON places (is_trending) WHERE is_trending;

CREATE TABLE IF NOT EXISTS foods (
	id          BIGSERIAL PRIMARY KEY,
	name        TEXT NOT NULL,
	country     TEXT NOT NULL,
	image_url   TEXT NOT NULL,
	is_popular  BOOLEAN NOT NULL DEFAULT false,
	description TEXT,
	source      TEXT NOT NULL DEFAULT 'seed',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS hotels (
	id           BIGSERIAL PRIMARY KEY,
	name         TEXT NOT NULL,
	place        TEXT NOT NULL,
	country      TEXT NOT NULL,
	region       TEXT,
	image_url    TEXT NOT NULL,
	gallery_urls TEXT[] NOT NULL DEFAULT '{}',
	rating       REAL NOT NULL DEFAULT 0,
	facilities   TEXT[] NOT NULL DEFAULT '{}',
	place_id     BIGINT REFERENCES places(id) ON DELETE SET NULL,
	source       TEXT NOT NULL DEFAULT 'seed',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_hotels_place ON hotels (place);

CREATE TABLE IF NOT EXISTS hotel_rooms (
	id          BIGSERIAL PRIMARY KEY,
	hotel_id    BIGINT NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	price_mmk   BIGINT NOT NULL,
	capacity    INT NOT NULL DEFAULT 2,
	image_url   TEXT NOT NULL,
	description TEXT
);
CREATE INDEX IF NOT EXISTS idx_hotel_rooms_hotel ON hotel_rooms (hotel_id);

CREATE TABLE IF NOT EXISTS bookings (
	id         BIGSERIAL PRIMARY KEY,
	user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	hotel_id   BIGINT NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
	hotel_name TEXT NOT NULL,
	room_name  TEXT NOT NULL,
	place      TEXT NOT NULL,
	image_url  TEXT NOT NULL,
	price_mmk  BIGINT NOT NULL,
	check_in   DATE NOT NULL,
	check_out  DATE NOT NULL,
	guests     INT NOT NULL DEFAULT 1,
	status     TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','complete','cancelled')),
	created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_bookings_user   ON bookings (user_id);
CREATE INDEX IF NOT EXISTS idx_bookings_status ON bookings (user_id, status);

CREATE TABLE IF NOT EXISTS favorites (
	id        BIGSERIAL PRIMARY KEY,
	user_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	place_id  BIGINT NOT NULL REFERENCES places(id) ON DELETE CASCADE,
	note      TEXT,
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	UNIQUE (user_id, place_id)
);
CREATE INDEX IF NOT EXISTS idx_favorites_user ON favorites (user_id);
`

// Migrate creates all tables/indexes (idempotent) and applies additive column
// migrations for databases created before Phase 2.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, schemaSQL)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	// Phase 2: additive migration for existing deployments.
	_, err = pool.Exec(ctx, `ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash TEXT NOT NULL DEFAULT ''`)
	if err != nil {
		return fmt.Errorf("migrate users.password_hash: %w", err)
	}
	// Firebase Auth: firebase_uid VARCHAR(128) UNIQUE NOT NULL. Existing
	// password-era rows get a stable 'legacy:<id>' placeholder (unique by
	// primary key) so the NOT NULL + UNIQUE constraints can be applied
	// without data loss; UpsertUserByFirebaseUID claims them by email on
	// the user's first Firebase sign-in.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS firebase_uid VARCHAR(128)`); err != nil {
		return fmt.Errorf("migrate users.firebase_uid add: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE users SET firebase_uid = 'legacy:' || id WHERE firebase_uid IS NULL`); err != nil {
		return fmt.Errorf("migrate users.firebase_uid backfill: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`ALTER TABLE users ALTER COLUMN firebase_uid SET NOT NULL`); err != nil {
		return fmt.Errorf("migrate users.firebase_uid not null: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS users_firebase_uid_key ON users (firebase_uid)`); err != nil {
		return fmt.Errorf("migrate users.firebase_uid unique: %w", err)
	}
	// Live-only content: list queries filter out rows whose source is
	// 'seed' (the mock dataset) so the app only ever shows provider-cached
	// rows; the constant DEFAULT backfills existing rows without a table
	// rewrite. Place/hotel provider writes and curated food inserts set
	// source explicitly so those live rows remain visible.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE places ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'seed'`); err != nil {
		return fmt.Errorf("migrate places.source: %w", err)
	}
	// Coordinates for "hotels near this POI" (place detail). Existing
	// provider-cached rows stay at 0,0 until re-cached; 0,0 is treated as
	// "unknown" everywhere.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE places ADD COLUMN IF NOT EXISTS lat DOUBLE PRECISION NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("migrate places.lat: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`ALTER TABLE places ADD COLUMN IF NOT EXISTS lon DOUBLE PRECISION NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("migrate places.lon: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`ALTER TABLE foods ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'seed'`); err != nil {
		return fmt.Errorf("migrate foods.source: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`ALTER TABLE hotels ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'seed'`); err != nil {
		return fmt.Errorf("migrate hotels.source: %w", err)
	}
	log.Println("[db] schema up to date")
	return nil
}

type seedPlace struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Country     string   `json:"country"`
	Region      string   `json:"region"`
	Category    string   `json:"category"`
	ImageURL    string   `json:"image_url"`
	GalleryURLs []string `json:"gallery_urls"`
	Rating      float64  `json:"rating"`
	IsPopular   bool     `json:"is_popular"`
	IsTrending  bool     `json:"is_trending"`
	Description string   `json:"description"`
}

type seedFood struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Country     string `json:"country"`
	ImageURL    string `json:"image_url"`
	IsPopular   bool   `json:"is_popular"`
	Description string `json:"description"`
}

type seedCategory struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Icon      string `json:"icon"`
	SortOrder int    `json:"sort_order"`
}

type seedRoom struct {
	Name        string `json:"name"`
	PriceMMK    int64  `json:"price_mmk"`
	Capacity    int    `json:"capacity"`
	ImageURL    string `json:"image_url"`
	Description string `json:"description"`
}

type seedHotel struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Place       string     `json:"place"`
	Country     string     `json:"country"`
	Region      string     `json:"region"`
	ImageURL    string     `json:"image_url"`
	GalleryURLs []string   `json:"gallery_urls"`
	Rating      float64    `json:"rating"`
	Facilities  []string   `json:"facilities"`
	Rooms       []seedRoom `json:"rooms"`
}

type seedBooking struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	HotelID   int64  `json:"hotel_id"`
	HotelName string `json:"hotel_name"`
	RoomName  string `json:"room_name"`
	Place     string `json:"place"`
	ImageURL  string `json:"image_url"`
	PriceMMK  int64  `json:"price_mmk"`
	CheckIn   string `json:"check_in"`
	CheckOut  string `json:"check_out"`
	Guests    int    `json:"guests"`
	Status    string `json:"status"`
}

type seedUser struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	Phone     string `json:"phone"`
	AvatarURL string `json:"avatar_url"`
	Bio       string `json:"bio"`
	Password  string `json:"password"`
}

type seedFavorite struct {
	ID      int64  `json:"id"`
	UserID  int64  `json:"user_id"`
	PlaceID int64  `json:"place_id"`
	Note    string `json:"note"`
}

// Seed checks each table and loads mock JSON when the table is empty.
func Seed(ctx context.Context, pool *pgxpool.Pool) error {
	dir := os.Getenv("MOCK_DATA_DIR")
	if dir == "" {
		dir = "mock_data"
	}
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		log.Printf("[seed] %s not found, skipping", dir)
		return nil
	}

	var err error
	if err = seedIfEmpty(ctx, pool, "users", filepath.Join(dir, "users.json"), seedUsers); err != nil {
		return err
	}
	// Backfill: rows seeded before Phase 2 have no password hash.
	if err = backfillUserPasswords(ctx, pool, filepath.Join(dir, "users.json")); err != nil {
		return err
	}
	if err = seedIfEmpty(ctx, pool, "categories", filepath.Join(dir, "categories.json"), seedCategories); err != nil {
		return err
	}
	if err = seedIfEmpty(ctx, pool, "places", filepath.Join(dir, "places.json"), seedPlaces); err != nil {
		return err
	}
	if err = seedIfEmpty(ctx, pool, "foods", filepath.Join(dir, "foods.json"), seedFoods); err != nil {
		return err
	}
	if err = seedIfEmpty(ctx, pool, "hotels", filepath.Join(dir, "hotels.json"), seedHotels); err != nil {
		return err
	}
	if err = seedIfEmpty(ctx, pool, "bookings", filepath.Join(dir, "bookings.json"), seedBookings); err != nil {
		return err
	}
	if err = seedIfEmpty(ctx, pool, "favorites", filepath.Join(dir, "favorites.json"), seedFavorites); err != nil {
		return err
	}
	return nil
}

func seedIfEmpty[T any](ctx context.Context, pool *pgxpool.Pool, table, file string, insert func(ctx context.Context, pool *pgxpool.Pool, items []T) error) error {
	var n int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		return fmt.Errorf("count %s: %w", table, err)
	}
	if n > 0 {
		return nil
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	var items []T
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("parse %s: %w", file, err)
	}
	if len(items) == 0 {
		return nil
	}
	if err := insert(ctx, pool, items); err != nil {
		return fmt.Errorf("seed %s: %w", table, err)
	}
	log.Printf("[seed] %s: inserted %d rows from %s", table, len(items), file)
	return nil
}

func seedUsers(ctx context.Context, pool *pgxpool.Pool, items []seedUser) error {
	for _, u := range items {
		hash, err := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash password for %s: %w", u.Email, err)
		}
		_, err = pool.Exec(ctx, `
			INSERT INTO users (id, name, email, phone, avatar_url, bio, password_hash, firebase_uid)
			VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),$7,'legacy:' || $1::text)
			ON CONFLICT (id) DO NOTHING`,
			u.ID, u.Name, u.Email, u.Phone, u.AvatarURL, u.Bio, string(hash))
		if err != nil {
			return err
		}
	}
	_, err := pool.Exec(ctx, `SELECT setval(pg_get_serial_sequence('users','id'), COALESCE((SELECT MAX(id) FROM users), 1))`)
	return err
}

// backfillUserPasswords assigns the JSON-file password to users whose
// password_hash is still empty (pre-Phase-2 rows).
func backfillUserPasswords(ctx context.Context, pool *pgxpool.Pool, file string) error {
	var n int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE password_hash = ''`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	var users []seedUser
	if err := json.Unmarshal(raw, &users); err != nil {
		return fmt.Errorf("parse %s: %w", file, err)
	}

	for _, u := range users {
		hash, err := bcrypt.GenerateFromPassword([]byte(u.Password), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `
			UPDATE users SET password_hash = $2
			WHERE email = $1 AND password_hash = ''`, u.Email, string(hash)); err != nil {
			return err
		}
	}
	log.Printf("[seed] users: backfilled password hashes for %d pre-Phase-2 rows", n)
	return nil
}

func seedCategories(ctx context.Context, pool *pgxpool.Pool, items []seedCategory) error {
	for _, c := range items {
		_, err := pool.Exec(ctx, `
			INSERT INTO categories (id, name, icon, sort_order)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (id) DO NOTHING`,
			c.ID, c.Name, c.Icon, c.SortOrder)
		if err != nil {
			return err
		}
	}
	_, err := pool.Exec(ctx, `SELECT setval(pg_get_serial_sequence('categories','id'), COALESCE((SELECT MAX(id) FROM categories), 1))`)
	return err
}

func seedPlaces(ctx context.Context, pool *pgxpool.Pool, items []seedPlace) error {
	for _, p := range items {
		_, err := pool.Exec(ctx, `
			INSERT INTO places (id, name, country, region, category, image_url, gallery_urls, rating, is_popular, is_trending, description)
			VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,$10,NULLIF($11,''))
			ON CONFLICT (id) DO NOTHING`,
			p.ID, p.Name, p.Country, p.Region, p.Category, p.ImageURL, p.GalleryURLs,
			p.Rating, p.IsPopular, p.IsTrending, p.Description)
		if err != nil {
			return err
		}
	}
	_, err := pool.Exec(ctx, `SELECT setval(pg_get_serial_sequence('places','id'), COALESCE((SELECT MAX(id) FROM places), 1))`)
	return err
}

func seedFoods(ctx context.Context, pool *pgxpool.Pool, items []seedFood) error {
	for _, f := range items {
		_, err := pool.Exec(ctx, `
			INSERT INTO foods (id, name, country, image_url, is_popular, description)
			VALUES ($1,$2,$3,$4,$5,NULLIF($6,''))
			ON CONFLICT (id) DO NOTHING`,
			f.ID, f.Name, f.Country, f.ImageURL, f.IsPopular, f.Description)
		if err != nil {
			return err
		}
	}
	_, err := pool.Exec(ctx, `SELECT setval(pg_get_serial_sequence('foods','id'), COALESCE((SELECT MAX(id) FROM foods), 1))`)
	return err
}

func seedHotels(ctx context.Context, pool *pgxpool.Pool, items []seedHotel) error {
	for _, h := range items {
		var placeID *int64
		_ = pool.QueryRow(ctx, `SELECT id FROM places WHERE name = $1 LIMIT 1`, h.Place).Scan(&placeID)

		_, err := pool.Exec(ctx, `
			INSERT INTO hotels (id, name, place, country, region, image_url, gallery_urls, rating, facilities, place_id)
			VALUES ($1,$2,$3,$4,NULLIF($5,''),$6,$7,$8,$9,$10)
			ON CONFLICT (id) DO NOTHING`,
			h.ID, h.Name, h.Place, h.Country, h.Region, h.ImageURL, h.GalleryURLs, h.Rating, h.Facilities, placeID)
		if err != nil {
			return err
		}
		for _, r := range h.Rooms {
			_, err := pool.Exec(ctx, `
				INSERT INTO hotel_rooms (hotel_id, name, price_mmk, capacity, image_url, description)
				VALUES ($1,$2,$3,$4,$5,NULLIF($6,''))
				ON CONFLICT DO NOTHING`,
				h.ID, r.Name, r.PriceMMK, r.Capacity, r.ImageURL, r.Description)
			if err != nil {
				return err
			}
		}
	}
	_, err := pool.Exec(ctx, `SELECT setval(pg_get_serial_sequence('hotels','id'), COALESCE((SELECT MAX(id) FROM hotels), 1))`)
	if err != nil {
		return err
	}
	return nil
}

func seedBookings(ctx context.Context, pool *pgxpool.Pool, items []seedBooking) error {
	for _, b := range items {
		_, err := pool.Exec(ctx, `
			INSERT INTO bookings (id, user_id, hotel_id, hotel_name, room_name, place, image_url, price_mmk, check_in, check_out, guests, status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::date,$10::date,$11,$12)
			ON CONFLICT (id) DO NOTHING`,
			b.ID, b.UserID, b.HotelID, b.HotelName, b.RoomName, b.Place, b.ImageURL,
			b.PriceMMK, b.CheckIn, b.CheckOut, b.Guests, b.Status)
		if err != nil {
			return err
		}
	}
	_, err := pool.Exec(ctx, `SELECT setval(pg_get_serial_sequence('bookings','id'), COALESCE((SELECT MAX(id) FROM bookings), 1))`)
	return err
}

func seedFavorites(ctx context.Context, pool *pgxpool.Pool, items []seedFavorite) error {
	for _, f := range items {
		_, err := pool.Exec(ctx, `
			INSERT INTO favorites (id, user_id, place_id, note)
			VALUES ($1,$2,$3,NULLIF($4,''))
			ON CONFLICT (id) DO NOTHING`,
			f.ID, f.UserID, f.PlaceID, f.Note)
		if err != nil {
			return err
		}
	}
	_, err := pool.Exec(ctx, `SELECT setval(pg_get_serial_sequence('favorites','id'), COALESCE((SELECT MAX(id) FROM favorites), 1))`)
	return err
}
