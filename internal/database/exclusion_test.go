package database

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// Phase 0 double-booking defense proofs, run against the real database
// (DATABASE_URL from ../../.env — the same Neon instance the server uses).
// Migrate() runs for real (it is idempotent); every write fixture happens
// inside transactions that are always rolled back, except
// TestConcurrentDoubleBooking which commits one clearly-marked fixture
// and deletes it in cleanup. All tests skip when DATABASE_URL is unset.

var (
	defenseOnce sync.Once
	defenseDB   *pgxpool.Pool
	defenseErr  error
)

func requireDefensePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	_ = godotenv.Load("../../.env")
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping double-booking defense tests")
	}
	defenseOnce.Do(func() {
		ctx := context.Background()
		defenseDB, defenseErr = NewPool(ctx)
		if defenseErr == nil {
			defenseErr = Migrate(ctx, defenseDB)
		}
	})
	if defenseErr != nil {
		t.Fatalf("prepare database: %v", defenseErr)
	}
	return defenseDB
}

// isExclusionErr reports SQLSTATE 23P01 (exclusion_violation) — the
// no_double_booking constraint rejecting a write.
func isExclusionErr(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}

// executor is satisfied by pgx.Tx and *pgxpool.Pool.
type executor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type fixture struct {
	tag     string
	userID  int64
	hotelID int64
	roomA   int64
	roomB   int64
}

func newFixture(t *testing.T, ctx context.Context, q executor) fixture {
	t.Helper()
	f := fixture{tag: fmt.Sprintf("defense-%d", time.Now().UnixNano())}
	if err := q.QueryRow(ctx, `
		INSERT INTO users (firebase_uid, name, email)
		VALUES ($1, 'defense-test', $2) RETURNING id`,
		"fb:"+f.tag, f.tag+"@test.local").Scan(&f.userID); err != nil {
		t.Fatalf("fixture user: %v", err)
	}
	if err := q.QueryRow(ctx, `
		INSERT INTO hotels (name, place, country, image_url)
		VALUES ($1, 'Defenseville', 'TT', '') RETURNING id`,
		"DEFENSE TEST "+f.tag).Scan(&f.hotelID); err != nil {
		t.Fatalf("fixture hotel: %v", err)
	}
	if err := q.QueryRow(ctx, `
		INSERT INTO hotel_rooms (hotel_id, name, price_mmk, image_url)
		VALUES ($1, 'Room A', 100000, '') RETURNING id`, f.hotelID).Scan(&f.roomA); err != nil {
		t.Fatalf("fixture room A: %v", err)
	}
	if err := q.QueryRow(ctx, `
		INSERT INTO hotel_rooms (hotel_id, name, price_mmk, image_url)
		VALUES ($1, 'Room B', 100000, '') RETURNING id`, f.hotelID).Scan(&f.roomB); err != nil {
		t.Fatalf("fixture room B: %v", err)
	}
	return f
}

func insertStay(ctx context.Context, q executor, f fixture, roomID int64, in, out, status string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO bookings (user_id, hotel_id, room_id, hotel_name, room_name, place, image_url,
		                      price_mmk, nights, total_mmk, check_in, check_out, guests, status)
		VALUES ($1,$2,$3,'DEFENSE','stay','Defenseville','',
		        100000,
		        GREATEST($5::date - $4::date, 1),
		        100000 * GREATEST($5::date - $4::date, 1),
		        $4::date, $5::date, 1, $6)`,
		f.userID, f.hotelID, roomID, in, out, status)
	return err
}

// TestMigrateInstallsBookingDefense asserts the Phase 0 schema contract:
// extension, constraint shape (checkout-exclusive, active statuses),
// room_id NOT NULL + FK, the status machine, and the users.role default.
func TestMigrateInstallsBookingDefense(t *testing.T) {
	pool := requireDefensePool(t)
	ctx := context.Background()

	var hasExt bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'btree_gist')`,
	).Scan(&hasExt); err != nil || !hasExt {
		t.Fatalf("btree_gist extension not installed (err=%v)", err)
	}

	var conDef string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conname = 'no_double_booking' AND conrelid = 'bookings'::regclass`,
	).Scan(&conDef); err != nil {
		t.Fatalf("no_double_booking constraint missing: %v", err)
	}
	// Checkout-exclusive '[)' — back-to-back stays must remain sellable.
	if !strings.Contains(conDef, `'[)'`) {
		t.Fatalf("no_double_booking must be checkout-exclusive '[)'): got %s", conDef)
	}
	for _, want := range []string{"room_id", "daterange", "pending", "confirmed"} {
		if !strings.Contains(conDef, want) {
			t.Fatalf("no_double_booking must mention %q: got %s", want, conDef)
		}
	}

	var notNull bool
	if err := pool.QueryRow(ctx, `
		SELECT attnotnull FROM pg_attribute
		WHERE attrelid = 'bookings'::regclass AND attname = 'room_id'`,
	).Scan(&notNull); err != nil || !notNull {
		t.Fatalf("bookings.room_id must be NOT NULL (err=%v) — check the migration WARNING for unmatched legacy rows", err)
	}

	var fkDef string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = 'bookings_room_id_fkey' AND conrelid = 'bookings'::regclass`,
	).Scan(&fkDef); err != nil {
		t.Fatalf("bookings.room_id FK missing: %v", err)
	}
	if !strings.Contains(fkDef, "hotel_rooms") {
		t.Fatalf("bookings_room_id_fkey must reference hotel_rooms: got %s", fkDef)
	}

	var statusDef string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conname = 'bookings_status_check' AND conrelid = 'bookings'::regclass`,
	).Scan(&statusDef); err != nil {
		t.Fatalf("bookings_status_check missing: %v", err)
	}
	if !strings.Contains(statusDef, "confirmed") {
		t.Fatalf("status machine must allow 'confirmed': got %s", statusDef)
	}

	var roleDef string
	if err := pool.QueryRow(ctx, `
		SELECT column_default FROM information_schema.columns
		WHERE table_name = 'users' AND column_name = 'role'`,
	).Scan(&roleDef); err != nil || !strings.Contains(roleDef, "user") {
		t.Fatalf("users.role must default to 'user' (err=%v, default=%q)", err, roleDef)
	}
}

// TestDoubleBookingDefense is the Data Integrity Audit: overlapping
// inserts fail gracefully with SQLSTATE 23P01 at the database level,
// while back-to-back stays, cancelled rows and other rooms stay legal.
// Runs in one transaction that is always rolled back.
func TestDoubleBookingDefense(t *testing.T) {
	pool := requireDefensePool(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	f := newFixture(t, ctx, tx)

	ok := func(room int64, in, out, status, why string) {
		t.Helper()
		if err := insertStay(ctx, tx, f, room, in, out, status); err != nil {
			t.Fatalf("%s: expected %s→%s (%s) to succeed, got: %v", why, in, out, status, err)
		}
	}
	// conflict expects the exclusion constraint to fire. Postgres aborts
	// the transaction on any error, so each attempt runs under a
	// savepoint — ROLLBACK TO SAVEPOINT keeps the fixture transaction
	// usable for the assertions that follow.
	conflict := func(room int64, in, out, status, why string) {
		t.Helper()
		if _, err := tx.Exec(ctx, `SAVEPOINT expect_conflict`); err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		err := insertStay(ctx, tx, f, room, in, out, status)
		if err == nil {
			t.Fatalf("%s: double booking %s→%s must be rejected, but the insert succeeded", why, in, out)
		}
		if !isExclusionErr(err) {
			t.Fatalf("%s: expected exclusion violation (SQLSTATE 23P01), got: %v", why, err)
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT expect_conflict`); err != nil {
			t.Fatalf("%s: rollback to savepoint: %v", why, err)
		}
	}

	// A: baseline pending hold.
	ok(f.roomA, "2027-01-01", "2027-01-03", "pending", "A baseline")
	// B: overlaps A by a night — Postgres itself must refuse it.
	conflict(f.roomA, "2027-01-02", "2027-01-04", "pending", "B overlaps A")
	// C: back-to-back turnover on A's checkout day — legal under '[)'.
	ok(f.roomA, "2027-01-03", "2027-01-05", "pending", "C back-to-back after A")
	// D: cancelled rows never hold inventory.
	ok(f.roomA, "2027-01-02", "2027-01-04", "cancelled", "D cancelled")
	// E: same dates on a different room — no cross-room coupling.
	ok(f.roomB, "2027-01-02", "2027-01-04", "pending", "E other room")

	// Confirmed bookings hold inventory too (Phase 3 transition).
	var idA int64
	if err := tx.QueryRow(ctx, `
		SELECT id FROM bookings
		WHERE user_id = $1 AND room_id = $2 AND check_in = '2027-01-01'
		LIMIT 1`, f.userID, f.roomA).Scan(&idA); err != nil {
		t.Fatalf("locate booking A: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE bookings SET status = 'confirmed' WHERE id = $1`, idA); err != nil {
		t.Fatalf("confirm A: %v", err)
	}
	conflict(f.roomA, "2027-01-02", "2027-01-04", "pending", "confirmed A still holds inventory")

	// Reschedule path: moving C onto A's (confirmed) range re-fires the
	// same constraint on UPDATE — 409s are produced by the DB, not Go.
	var idC int64
	if err := tx.QueryRow(ctx, `
		SELECT id FROM bookings
		WHERE user_id = $1 AND room_id = $2 AND check_in = '2027-01-03'
		LIMIT 1`, f.userID, f.roomA).Scan(&idC); err != nil {
		t.Fatalf("locate booking C: %v", err)
	}
	_, err = tx.Exec(ctx, `
		UPDATE bookings SET check_in = '2027-01-01', check_out = '2027-01-03' WHERE id = $1`, idC)
	if err == nil {
		t.Fatal("rescheduling onto a held range must be rejected")
	}
	if !isExclusionErr(err) {
		t.Fatalf("reschedule must fail with SQLSTATE 23P01, got: %v", err)
	}
}

// TestConcurrentDoubleBooking proves the race-condition guarantee: two
// simultaneous transactions targeting the same room/dates — exactly one
// wins, the loser gets 23P01. The constraint arbitrates inside Postgres,
// so no application-level locking is involved.
func TestConcurrentDoubleBooking(t *testing.T) {
	pool := requireDefensePool(t)
	ctx := context.Background()

	// Committed fixture: two connections cannot share one transaction.
	// Clearly marked rows; deleted in cleanup.
	f := newFixture(t, ctx, pool)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(cctx, `DELETE FROM bookings WHERE user_id = $1`, f.userID)
		_, _ = pool.Exec(cctx, `DELETE FROM hotel_rooms WHERE hotel_id = $1`, f.hotelID)
		_, _ = pool.Exec(cctx, `DELETE FROM hotels WHERE id = $1`, f.hotelID)
		_, _ = pool.Exec(cctx, `DELETE FROM users WHERE id = $1`, f.userID)
	})

	const attempts = 2
	results := make([]error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conn, err := pool.Acquire(ctx)
			if err != nil {
				results[i] = err
				return
			}
			defer conn.Release()
			tx, err := conn.Begin(ctx)
			if err != nil {
				results[i] = err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if err := insertStay(ctx, tx, f, f.roomA, "2027-06-01", "2027-06-03", "pending"); err != nil {
				results[i] = err
				return
			}
			results[i] = tx.Commit(ctx)
		}(i)
	}
	wg.Wait()

	var wins, conflicts int
	for i, err := range results {
		switch {
		case err == nil:
			wins++
		case isExclusionErr(err):
			conflicts++
		default:
			t.Fatalf("attempt %d: unexpected error: %v", i, err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("exactly one concurrent booking must win — got %d win(s), %d conflict(s): %v",
			wins, conflicts, results)
	}
}
