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
	role          TEXT NOT NULL DEFAULT 'user',
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
	description TEXT,
	total_units INT NOT NULL DEFAULT 1,
	is_active   BOOLEAN NOT NULL DEFAULT true
);
CREATE INDEX IF NOT EXISTS idx_hotel_rooms_hotel ON hotel_rooms (hotel_id);

CREATE TABLE IF NOT EXISTS bookings (
	id           BIGSERIAL PRIMARY KEY,
	user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	hotel_id     BIGINT NOT NULL REFERENCES hotels(id) ON DELETE CASCADE,
	room_id      BIGINT REFERENCES hotel_rooms(id) ON DELETE RESTRICT,
	hotel_name   TEXT NOT NULL,
	room_name    TEXT NOT NULL,
	place        TEXT NOT NULL,
	image_url    TEXT NOT NULL,
	price_mmk    BIGINT NOT NULL,
	nights       INT NOT NULL DEFAULT 1,
	total_mmk    BIGINT NOT NULL DEFAULT 0,
	guest_name   TEXT,
	guest_phone  TEXT,
	check_in     DATE NOT NULL,
	check_out    DATE NOT NULL,
	guests       INT NOT NULL DEFAULT 1,
	status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','confirmed','complete','cancelled')),
	confirmed_at TIMESTAMPTZ,
	cancelled_at TIMESTAMPTZ,
	created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
	CONSTRAINT bookings_check_dates CHECK (check_out > check_in)
);
CREATE INDEX IF NOT EXISTS idx_bookings_user   ON bookings (user_id);
CREATE INDEX IF NOT EXISTS idx_bookings_status ON bookings (user_id, status);

CREATE TABLE IF NOT EXISTS admin_audit_logs (
	id          BIGSERIAL PRIMARY KEY,
	admin_id    BIGINT REFERENCES users(id) ON DELETE SET NULL,
	action      TEXT NOT NULL,
	entity_type TEXT NOT NULL,
	entity_id   BIGINT NOT NULL,
	details     JSONB NOT NULL DEFAULT '{}',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_admin_audit_logs_created ON admin_audit_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_logs_entity  ON admin_audit_logs (entity_type, entity_id);

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

	// ── Phase 0: booking inventory hardening + double-booking defense ──
	//
	// hotel_rooms: allotment count (units of this room type) and a
	// stop-sell switch — the inventory inputs Phase 1/2 availability reads.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE hotel_rooms ADD COLUMN IF NOT EXISTS total_units INT NOT NULL DEFAULT 1`); err != nil {
		return fmt.Errorf("migrate hotel_rooms.total_units: %w", err)
	}
	if _, err := pool.Exec(ctx,
		`ALTER TABLE hotel_rooms ADD COLUMN IF NOT EXISTS is_active BOOLEAN NOT NULL DEFAULT true`); err != nil {
		return fmt.Errorf("migrate hotel_rooms.is_active: %w", err)
	}
	// bookings: room identity + pricing/guest/timestamp columns. Added
	// nullable first so existing rows never block the migration; NOT NULL
	// is applied below only once every legacy row has been matched.
	for _, col := range []struct{ name, ddl string }{
		{"room_id", "BIGINT"},
		{"nights", "INT"},
		{"total_mmk", "BIGINT"},
		{"guest_name", "TEXT"},
		{"guest_phone", "TEXT"},
		{"confirmed_at", "TIMESTAMPTZ"},
		{"cancelled_at", "TIMESTAMPTZ"},
	} {
		if _, err := pool.Exec(ctx,
			`ALTER TABLE bookings ADD COLUMN IF NOT EXISTS `+col.name+` `+col.ddl); err != nil {
			return fmt.Errorf("migrate bookings.%s: %w", col.name, err)
		}
	}
	// Room-occupancy lookups (Phase 2 availability) filter by room_id.
	// Lives here, not in schemaSQL: CREATE INDEX in the idempotent schema
	// would run before the ADD COLUMN above on pre-Phase-0 databases.
	if _, err := pool.Exec(ctx,
		`CREATE INDEX IF NOT EXISTS idx_bookings_room ON bookings (room_id)`); err != nil {
		return fmt.Errorf("migrate bookings.room_id index: %w", err)
	}
	// room_id backfill: the pre-Phase-0 schema only kept room_name, so the
	// only possible join is (hotel_id, room_name).
	if _, err := pool.Exec(ctx, `
		UPDATE bookings b
		SET room_id = r.id
		FROM hotel_rooms r
		WHERE b.room_id IS NULL
		  AND r.hotel_id = b.hotel_id
		  AND r.name = b.room_name`); err != nil {
		return fmt.Errorf("migrate bookings.room_id backfill: %w", err)
	}
	var unmatched int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM bookings WHERE room_id IS NULL`).Scan(&unmatched); err != nil {
		return fmt.Errorf("migrate bookings.room_id audit: %w", err)
	}
	if unmatched == 0 {
		if _, err := pool.Exec(ctx,
			`ALTER TABLE bookings ALTER COLUMN room_id SET NOT NULL`); err != nil {
			return fmt.Errorf("migrate bookings.room_id not null: %w", err)
		}
	} else {
		log.Printf("[db] WARNING: %d legacy booking(s) reference deleted rooms — room_id stays nullable; the exclusion constraint still protects every matched row", unmatched)
	}
	// FK: a booking may never outlive its room row (RESTRICT keeps the
	// snapshot honest instead of silently cascading bookings away).
	if _, err := pool.Exec(ctx, `
		DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conrelid = 'bookings'::regclass
				  AND conname = 'bookings_room_id_fkey'
			) THEN
				ALTER TABLE bookings ADD CONSTRAINT bookings_room_id_fkey
					FOREIGN KEY (room_id) REFERENCES hotel_rooms(id) ON DELETE RESTRICT;
			END IF;
		END $$;`); err != nil {
		return fmt.Errorf("migrate bookings.room_id fk: %w", err)
	}
	// Legacy pricing backfill: nights = stay length; total_mmk = the stored
	// price. Pre-Phase-0 price_mmk was never multiplied by nights, so
	// treating it as the total preserves exactly what the app already
	// displayed — no inflation, no guessing. New writes compute rate × nights.
	if _, err := pool.Exec(ctx, `
		UPDATE bookings
		SET nights    = GREATEST(check_out - check_in, 1),
		    total_mmk = price_mmk
		WHERE nights IS NULL`); err != nil {
		return fmt.Errorf("migrate bookings nights/total backfill: %w", err)
	}
	// Date sanity: a zero/negative stay can never hold inventory.
	if _, err := pool.Exec(ctx, `
		DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conrelid = 'bookings'::regclass
				  AND conname = 'bookings_check_dates'
			) THEN
				ALTER TABLE bookings ADD CONSTRAINT bookings_check_dates
					CHECK (check_out > check_in);
			END IF;
		END $$;`); err != nil {
		return fmt.Errorf("migrate bookings.check_dates: %w", err)
	}
	// Status machine: 'confirmed' joins pending/complete/cancelled (Phase 3
	// drives pending → confirmed → complete). Every status CHECK is dropped
	// first so a stale narrower definition can never reject the new transitions.
	if _, err := pool.Exec(ctx, `
		DO $$
		DECLARE c RECORD;
		BEGIN
			FOR c IN (
				SELECT conname FROM pg_constraint
				WHERE conrelid = 'bookings'::regclass
				  AND contype = 'c'
				  AND pg_get_constraintdef(oid) LIKE '%status%'
			) LOOP
				EXECUTE format('ALTER TABLE bookings DROP CONSTRAINT %I', c.conname);
			END LOOP;
			ALTER TABLE bookings ADD CONSTRAINT bookings_status_check
				CHECK (status IN ('pending','confirmed','complete','cancelled'));
		END $$;`); err != nil {
		return fmt.Errorf("migrate bookings.status check: %w", err)
	}
	// Double-booking defense: btree_gist gives GiST the `=` operator on
	// bigint room_id; the EXCLUDE constraint then rejects any two active
	// bookings whose [check_in, check_out) windows overlap for the same
	// room. Enforcement lives in Postgres itself, so concurrent requests
	// and reschedules are race-proof without application-level locking.
	//
	// Checkout-exclusive '[)' is the hotel-industry standard (Agoda,
	// Booking.com): a stay occupies the nights [check_in, check_out) and
	// the room turns over on checkout morning — back-to-back stays
	// (Jan 1-3 then Jan 3-5) are legal and sellable. Revert to '[]' to
	// also block same-day turnovers (one line, at the cost of inventory).
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS btree_gist`); err != nil {
		return fmt.Errorf("migrate enable btree_gist: %w", err)
	}
	// Refuse to install the defense over data that already violates it:
	// overlapping active pairs must be resolved by an operator first.
	rows, err := pool.Query(ctx, `
		SELECT b1.id, b2.id
		FROM bookings b1
		JOIN bookings b2
		  ON b1.id < b2.id
		 AND b1.room_id = b2.room_id
		 AND b1.status IN ('pending','confirmed')
		 AND b2.status IN ('pending','confirmed')
		 AND b1.check_in < b2.check_out
		 AND b2.check_in < b1.check_out
		WHERE b1.room_id IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("migrate overlap audit: %w", err)
	}
	var pairs [][2]int64
	for rows.Next() {
		var p [2]int64
		if err := rows.Scan(&p[0], &p[1]); err != nil {
			rows.Close()
			return fmt.Errorf("migrate overlap audit scan: %w", err)
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("migrate overlap audit: %w", err)
	}
	if len(pairs) > 0 {
		return fmt.Errorf(
			"migrate: %d overlapping active booking pair(s) block the no_double_booking constraint (booking ids: %v) — cancel or reschedule one booking of each pair, then restart",
			len(pairs), pairs)
	}
	if _, err := pool.Exec(ctx, `
		DO $$
		BEGIN
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conrelid = 'bookings'::regclass
				  AND conname = 'no_double_booking'
			) THEN
				ALTER TABLE bookings ADD CONSTRAINT no_double_booking
					EXCLUDE USING gist (
						room_id WITH =,
						daterange(check_in, check_out, '[)') WITH &&
					)
					WHERE (status IN ('pending','confirmed'));
			END IF;
		END $$;`); err != nil {
		return fmt.Errorf("migrate no_double_booking constraint: %w", err)
	}
	// users.role: RBAC column enforced by AdminRequired (Phase 1 extranet).
	if _, err := pool.Exec(ctx,
		`ALTER TABLE users ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'user'`); err != nil {
		return fmt.Errorf("migrate users.role: %w", err)
	}
	// ── Phase 2: multi-unit inventory — unit-scoped exclusion ──────────
	//
	// Phase 0's constraint allowed ONE active booking per room row, but a
	// room type can sell several identical units (hotel_rooms.total_units).
	// EXCLUDE cannot count cardinality, so each sold unit gets a slot:
	// unit_no in [1..total_units]. The defense becomes (room, unit, range)
	// — two guests may hold the same room type on the same dates as long
	// as they hold DIFFERENT units, and no unit is ever double-booked.
	//
	// All pre-Phase-2 rows get unit_no = 1; the old room-level constraint
	// already guaranteed they never overlap, so the new (weaker-per-room,
	// stronger-per-unit) constraint is satisfied by construction.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE bookings ADD COLUMN IF NOT EXISTS unit_no INT NOT NULL DEFAULT 1`); err != nil {
		return fmt.Errorf("migrate bookings.unit_no: %w", err)
	}
	// Atomic swap inside one DO block (one transaction): drop the old
	// room-level constraint and install the unit-scoped one, so a crash
	// can never leave the table with no defense at all.
	if _, err := pool.Exec(ctx, `
		DO $$
		BEGIN
			IF EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conrelid = 'bookings'::regclass
				  AND conname = 'no_double_booking'
			) THEN
				ALTER TABLE bookings DROP CONSTRAINT no_double_booking;
			END IF;
			IF NOT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conrelid = 'bookings'::regclass
				  AND conname = 'no_double_booking'
			) THEN
				ALTER TABLE bookings ADD CONSTRAINT no_double_booking
					EXCLUDE USING gist (
						room_id WITH =,
						unit_no WITH =,
						daterange(check_in, check_out, '[)') WITH &&
					)
					WHERE (status IN ('pending','confirmed'));
			END IF;
		END $$;`); err != nil {
		return fmt.Errorf("migrate no_double_booking v2 (unit-scoped): %w", err)
	}
	// Unit-occupancy lookups (availability engine + unit assignment).
	if _, err := pool.Exec(ctx,
		`CREATE INDEX IF NOT EXISTS idx_bookings_room_unit
		 ON bookings (room_id, unit_no) WHERE status IN ('pending','confirmed')`); err != nil {
		return fmt.Errorf("migrate bookings.room_unit index: %w", err)
	}
	log.Println("[db] unit-scoped double-booking defense active (room, unit, range)")

	// ── Phase 3: pay-at-property — payment_method + born-confirmed ─────
	//
	// Pay-at-property moves NO money online, so a successful booking is
	// born 'confirmed' (not a tentative 'pending' hold). The column records
	// HOW the stay will be paid so the app can render the "Pay at
	// Accommodation" badge from stored truth, and a future online-gateway
	// phase gets a first-class value ('card'/'wallet') to branch on instead
	// of overloading the status machine.
	//
	// NOT NULL DEFAULT keeps every pre-Phase-3 row (and any row inserted
	// by an older app version mid-deploy) valid: their payment method was
	// pay-at-property in practice. No backfill UPDATE needed.
	if _, err := pool.Exec(ctx,
		`ALTER TABLE bookings ADD COLUMN IF NOT EXISTS payment_method TEXT NOT NULL DEFAULT 'pay_at_property'`); err != nil {
		return fmt.Errorf("migrate bookings.payment_method: %w", err)
	}
	log.Println("[db] pay-at-property payment_method active (bookings born confirmed)")
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
		// Phase 0: room_id resolves through (hotel_id, room_name) — the
		// only identity the seed JSON carries — and nights/total follow
		// the rate × nights rule. Rows whose room is missing are skipped:
		// they could not be protected by the exclusion constraint anyway.
		_, err := pool.Exec(ctx, `
			INSERT INTO bookings (id, user_id, hotel_id, hotel_name, room_name, place, image_url,
			                      price_mmk, check_in, check_out, guests, status,
			                      room_id, nights, total_mmk)
			SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9::date,$10::date,$11,$12,
			       r.id,
			       GREATEST($10::date - $9::date, 1),
			       $8 * GREATEST($10::date - $9::date, 1)
			FROM hotel_rooms r
			WHERE r.hotel_id = $3 AND r.name = $5
			ORDER BY r.id
			LIMIT 1
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
