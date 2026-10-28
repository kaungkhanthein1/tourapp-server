package bookings

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"travel-app-backend/internal/database"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// Phase 2 availability + multi-unit inventory proofs, run against the
// real database (DATABASE_URL from ../../.env). Every write fixture
// happens inside transactions that are always rolled back, except the
// concurrency test which commits one clearly-marked fixture and deletes
// it in cleanup. All tests skip when DATABASE_URL is unset.

var (
	availOnce sync.Once
	availDB   *pgxpool.Pool
	availErr  error
)

func requireAvailDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	_ = godotenv.Load("../../.env")
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping availability tests")
	}
	availOnce.Do(func() {
		ctx := context.Background()
		availDB, availErr = database.NewPool(ctx)
		if availErr == nil {
			availErr = database.Migrate(ctx, availDB)
		}
	})
	if availErr != nil {
		t.Fatalf("prepare database: %v", availErr)
	}
	return availDB
}

// avFixture: admin-less fixture — one hotel, one room with a chosen
// unit count, one guest user.
type avFixture struct {
	tag     string
	userID  int64
	hotelID int64
	roomID  int64
	units   int
}

func newAvFixture(t *testing.T, ctx context.Context, q dbtx, units int) avFixture {
	t.Helper()
	f := avFixture{tag: fmt.Sprintf("avail-%d", time.Now().UnixNano()), units: units}
	if err := q.QueryRow(ctx, `
		INSERT INTO users (firebase_uid, name, email)
		VALUES ($1, 'avail-test', $2) RETURNING id`,
		"fb:"+f.tag, f.tag+"@test.local").Scan(&f.userID); err != nil {
		t.Fatalf("fixture user: %v", err)
	}
	if err := q.QueryRow(ctx, `
		INSERT INTO hotels (name, place, country, image_url)
		VALUES ($1, 'Availville', 'TT', '') RETURNING id`,
		"AVAIL TEST "+f.tag).Scan(&f.hotelID); err != nil {
		t.Fatalf("fixture hotel: %v", err)
	}
	if err := q.QueryRow(ctx, `
		INSERT INTO hotel_rooms (hotel_id, name, price_mmk, image_url, total_units)
		VALUES ($1, 'Avail Room', 100000, '', $2) RETURNING id`,
		f.hotelID, units).Scan(&f.roomID); err != nil {
		t.Fatalf("fixture room: %v", err)
	}
	return f
}

// bookViaService runs the real create path (unit assignment included).
func bookViaService(t *testing.T, f avFixture, svc *Service, in, out string) (*Booking, error) {
	t.Helper()
	return svc.Create(context.Background(), f.userID, CreateRequest{
		HotelID: f.hotelID, RoomID: f.roomID, CheckIn: in, CheckOut: out, Guests: 1,
	})
}

// TestMultiUnitBooking proves the Phase 2 core: a room type with N units
// sells up to N overlapping stays (one per unit), and the (N+1)th is
// rejected. Runs in one always-rolled-back transaction.
func TestMultiUnitBooking(t *testing.T) {
	pool := requireAvailDB(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	f := newAvFixture(t, ctx, tx, 3)
	svc := NewService(&PostgresRepository{db: tx})

	// Three overlapping stays on the SAME room type — all must succeed,
	// each holding a distinct unit.
	var unitHolds []int
	for i := 0; i < 3; i++ {
		b, err := bookViaService(t, f, svc, "2027-11-01", "2027-11-04")
		if err != nil {
			t.Fatalf("overlapping stay %d must succeed (3 units): %v", i+1, err)
		}
		unitHolds = append(unitHolds, b.UnitNo)
	}
	seen := map[int]bool{}
	for _, u := range unitHolds {
		if u < 1 || u > 3 {
			t.Fatalf("unit_no %d out of range [1..3]", u)
		}
		if seen[u] {
			t.Fatalf("unit %d assigned twice for overlapping stays: %v", u, unitHolds)
		}
		seen[u] = true
	}

	// The 4th overlapping stay has no free unit → ErrNotAvailable.
	if _, err := bookViaService(t, f, svc, "2027-11-02", "2027-11-05"); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("4th overlapping stay must be ErrNotAvailable, got: %v", err)
	}

	// A stay AFTER the checkout day frees every unit (back-to-back legal).
	if _, err := bookViaService(t, f, svc, "2027-11-04", "2027-11-06"); err != nil {
		t.Fatalf("back-to-back stay after checkout must succeed: %v", err)
	}

	// Cancelling one hold frees its unit for a new overlapping guest.
	// The back-to-back stay landed on the lowest unit (unit 1) for Nov 4-6;
	// cancel one of the ORIGINAL Nov 1-4 holds on a DIFFERENT unit, then
	// book nights 2-3 (which never touch the back-to-back's Nov 4 night).
	var backUnit int
	if err := tx.QueryRow(ctx, `
		SELECT unit_no FROM bookings
		WHERE room_id = $1 AND check_in = '2027-11-04' AND status IN ('pending','confirmed')`, f.roomID).Scan(&backUnit); err != nil {
		t.Fatalf("locate back-to-back stay: %v", err)
	}
	var cancelID int64
	if err := tx.QueryRow(ctx, `
		SELECT id FROM bookings
		WHERE room_id = $1 AND unit_no <> $2 AND check_in = '2027-11-01' AND status IN ('pending','confirmed')
		LIMIT 1`, f.roomID, backUnit).Scan(&cancelID); err != nil {
		t.Fatalf("locate hold: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE bookings SET status = 'cancelled', cancelled_at = now() WHERE id = $1`, cancelID); err != nil {
		t.Fatalf("cancel hold: %v", err)
	}
	if _, err := bookViaService(t, f, svc, "2027-11-02", "2027-11-04"); err != nil {
		t.Fatalf("cancelled unit must be rebookable, got: %v", err)
	}
}

// TestStopSellRejectsBooking proves the admin stop-sell switch: an
// inactive room refuses new bookings with ErrRoomInactive (409).
func TestStopSellRejectsBooking(t *testing.T) {
	pool := requireAvailDB(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	f := newAvFixture(t, ctx, tx, 2)
	svc := NewService(&PostgresRepository{db: tx})

	// Admin flips stop-sell.
	if _, err := tx.Exec(ctx,
		`UPDATE hotel_rooms SET is_active = false WHERE id = $1`, f.roomID); err != nil {
		t.Fatalf("stop-sell: %v", err)
	}
	if _, err := bookViaService(t, f, svc, "2027-12-01", "2027-12-03"); !errors.Is(err, ErrRoomInactive) {
		t.Fatalf("stop-sold room must reject with ErrRoomInactive, got: %v", err)
	}

	// Re-open → booking succeeds again.
	if _, err := tx.Exec(ctx,
		`UPDATE hotel_rooms SET is_active = true WHERE id = $1`, f.roomID); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := bookViaService(t, f, svc, "2027-12-01", "2027-12-03"); err != nil {
		t.Fatalf("reopened room must book: %v", err)
	}
}

// TestAvailabilityEngine proves the read model: peak-night occupancy,
// free units, bookable flag, and the hotel-not-found path.
func TestAvailabilityEngine(t *testing.T) {
	pool := requireAvailDB(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	f := newAvFixture(t, ctx, tx, 4)
	svc := NewService(&PostgresRepository{db: tx})

	// Room A (the fixture room) bookings: two overlapping (Nov 1-3, Nov 2-4)
	// → peak = 2 on Nov 2; a third non-overlapping (Nov 5-7) stays out of peak.
	for _, stay := range [][2]string{{"2027-11-01", "2027-11-03"}, {"2027-11-02", "2027-11-04"}} {
		if _, err := bookViaService(t, f, svc, stay[0], stay[1]); err != nil {
			t.Fatalf("setup booking %v: %v", stay, err)
		}
	}
	// A second room type with 1 unit, fully sold for the window.
	var roomB int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO hotel_rooms (hotel_id, name, price_mmk, image_url, total_units)
		VALUES ($1, 'Single Unit Room', 50000, '', 1) RETURNING id`, f.hotelID).Scan(&roomB); err != nil {
		t.Fatalf("room B: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO bookings (user_id, hotel_id, room_id, hotel_name, room_name, place, image_url,
		                      price_mmk, nights, total_mmk, check_in, check_out, guests, status, unit_no)
		VALUES ($1,$2,$3,'AVAIL','Single Unit Room','Availville','',50000,2,100000,'2027-11-01','2027-11-03',1,'pending',1)`,
		f.userID, f.hotelID, roomB); err != nil {
		t.Fatalf("room B booking: %v", err)
	}
	// A stop-sold third room.
	var roomC int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO hotel_rooms (hotel_id, name, price_mmk, image_url, total_units, is_active)
		VALUES ($1, 'Stop Sold', 10000, '', 5, false) RETURNING id`, f.hotelID).Scan(&roomC); err != nil {
		t.Fatalf("room C: %v", err)
	}

	resp, err := svc.Availability(ctx, f.hotelID, "2027-11-01", "2027-11-04")
	if err != nil {
		t.Fatalf("availability: %v", err)
	}
	if resp.Nights != 3 || resp.CheckIn != "2027-11-01" || resp.CheckOut != "2027-11-04" {
		t.Fatalf("response header wrong: %+v", resp)
	}
	if len(resp.Rooms) != 3 {
		t.Fatalf("expected 3 rooms, got %d", len(resp.Rooms))
	}

	byID := map[int64]RoomAvailability{}
	for _, r := range resp.Rooms {
		byID[r.RoomID] = r
	}

	a := byID[f.roomID]
	if a.UnitsBooked != 2 || a.UnitsFree != 2 || !a.Bookable {
		t.Fatalf("room A: want booked=2 free=2 bookable=true, got %+v", a)
	}
	b := byID[roomB]
	if b.UnitsBooked != 1 || b.UnitsFree != 0 || b.Bookable {
		t.Fatalf("room B (sold out): want booked=1 free=0 bookable=false, got %+v", b)
	}
	c := byID[roomC]
	if c.Bookable || c.IsActive {
		t.Fatalf("room C (stop-sold): want bookable=false is_active=false, got %+v", c)
	}

	// Shift the window to a free range → room A peak drops.
	resp2, err := svc.Availability(ctx, f.hotelID, "2027-11-04", "2027-11-06")
	if err != nil {
		t.Fatalf("availability 2: %v", err)
	}
	for _, r := range resp2.Rooms {
		if r.RoomID == f.roomID && (r.UnitsBooked != 0 || r.UnitsFree != 4) {
			t.Fatalf("room A on free window: want booked=0 free=4, got %+v", r)
		}
	}

	// Missing hotel → ErrHotelNotFound (handler maps to 404).
	if _, err := svc.Availability(ctx, 999999999, "2027-11-01", "2027-11-04"); !errors.Is(err, ErrHotelNotFound) {
		t.Fatalf("missing hotel must be ErrHotelNotFound, got: %v", err)
	}
	// Bad dates → ErrInvalidDates.
	if _, err := svc.Availability(ctx, f.hotelID, "2027-11-04", "2027-11-01"); !errors.Is(err, ErrInvalidDates) {
		t.Fatalf("reversed dates must be ErrInvalidDates, got: %v", err)
	}
}

// TestRescheduleUnitReassignment proves that moving a booking to dates
// where its own unit is taken reassigns it to a free unit (and that a
// fully-blocked target is rejected).
func TestRescheduleUnitReassignment(t *testing.T) {
	pool := requireAvailDB(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	f := newAvFixture(t, ctx, tx, 2)
	svc := NewService(&PostgresRepository{db: tx})

	// Two overlapping stays fill both units for Nov 10-12.
	s1, err := bookViaService(t, f, svc, "2027-11-10", "2027-11-12")
	if err != nil {
		t.Fatalf("stay 1: %v", err)
	}
	if _, err := bookViaService(t, f, svc, "2027-11-10", "2027-11-12"); err != nil {
		t.Fatalf("stay 2: %v", err)
	}

	// A third stay sits alone on Nov 20-22 (its unit is free there).
	s3, err := bookViaService(t, f, svc, "2027-11-20", "2027-11-22")
	if err != nil {
		t.Fatalf("stay 3: %v", err)
	}

	// Reschedule s3 ONTO the full window: both units are held → reject.
	if _, err := svc.Reschedule(ctx, f.userID, s3.ID, "2027-11-10", "2027-11-12"); !errors.Is(err, ErrNotAvailable) {
		t.Fatalf("reschedule onto a full window must be ErrNotAvailable, got: %v", err)
	}

	// Cancel s1 (frees one unit on the full window), then reschedule s3
	// there: it must land on a free unit even if its old one differs.
	if _, err := svc.Cancel(ctx, f.userID, s1.ID); err != nil {
		t.Fatalf("cancel s1: %v", err)
	}
	moved, err := svc.Reschedule(ctx, f.userID, s3.ID, "2027-11-10", "2027-11-12")
	if err != nil {
		t.Fatalf("reschedule into freed unit: %v", err)
	}
	if moved.CheckIn != "2027-11-10" || moved.CheckOut != "2027-11-12" {
		t.Fatalf("dates not applied: %+v", moved)
	}
	// The surviving overlapping hold + the moved one must use distinct units.
	var units []int
	rows, err := tx.Query(ctx, `
		SELECT unit_no FROM bookings
		WHERE room_id = $1 AND status IN ('pending','confirmed')
		  AND check_in < '2027-11-12' AND check_out > '2027-11-10'
		ORDER BY unit_no`, f.roomID)
	if err != nil {
		t.Fatalf("read units: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var u int
		if err := rows.Scan(&u); err != nil {
			t.Fatalf("scan unit: %v", err)
		}
		units = append(units, u)
	}
	if len(units) != 2 || units[0] == units[1] {
		t.Fatalf("expected 2 distinct units on the window, got %v", units)
	}
}

// TestConcurrentMultiUnitRace proves the race guarantee at unit
// granularity: 5 concurrent guests race for 3 units of the same room and
// dates — exactly 3 win, exactly 2 get ErrNotAvailable. The FOR UPDATE
// serialization + unit scan decide; no application-level global lock.
func TestConcurrentMultiUnitRace(t *testing.T) {
	pool := requireAvailDB(t)
	ctx := context.Background()

	// Committed fixture (concurrent goroutines need separate conns).
	f := newAvFixture(t, ctx, pool, 3)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = pool.Exec(cctx, `DELETE FROM bookings WHERE user_id = $1`, f.userID)
		_, _ = pool.Exec(cctx, `DELETE FROM hotel_rooms WHERE hotel_id = $1`, f.hotelID)
		_, _ = pool.Exec(cctx, `DELETE FROM hotels WHERE id = $1`, f.hotelID)
		_, _ = pool.Exec(cctx, `DELETE FROM users WHERE id = $1`, f.userID)
	})

	const racers = 5
	svc := NewService(NewPostgresRepository(pool))
	results := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.Create(ctx, f.userID, CreateRequest{
				HotelID: f.hotelID, RoomID: f.roomID,
				CheckIn: "2027-06-01", CheckOut: "2027-06-03", Guests: 1,
			})
			results[i] = err
		}(i)
	}
	wg.Wait()

	var wins, sold int
	for i, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrNotAvailable):
			sold++
		default:
			t.Fatalf("racer %d: unexpected error: %v", i, err)
		}
	}
	if wins != 3 || sold != 2 {
		t.Fatalf("exactly 3 of 5 racers must win (3 units) — got %d win(s), %d sold-out: %v",
			wins, sold, results)
	}

	// The DB agrees: 3 rows, 3 distinct units, none overlapping — and every
	// winner is BORN CONFIRMED (pay-at-property moves no online money).
	var distinct, total, confirmed int
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT unit_no), count(*), count(*) FILTER (WHERE status = 'confirmed')
		FROM bookings WHERE room_id = $1 AND status IN ('pending','confirmed')`, f.roomID,
	).Scan(&distinct, &total, &confirmed); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if distinct != 3 || total != 3 {
		t.Fatalf("expected 3 bookings on 3 distinct units, got %d distinct / %d total", distinct, total)
	}
	if confirmed != 3 {
		t.Fatalf("pay-at-property bookings must be born confirmed, got %d of %d", confirmed, total)
	}
}

// TestPayAtPropertyLifecycle proves the Phase 3 modified flow end to end:
// a pay-at-property booking is BORN CONFIRMED (no tentative hold), stores
// its payment method, is cancellable/reschedulable by the owner while
// upcoming, appears under the mobile "Pending" tab's upcoming filter, and
// rejects unsupported payment methods with no row written.
func TestPayAtPropertyLifecycle(t *testing.T) {
	pool := requireAvailDB(t)
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	f := newAvFixture(t, ctx, tx, 2)
	svc := NewService(&PostgresRepository{db: tx})

	// 1) Create with payment_method omitted → born confirmed, default method.
	b, err := svc.Create(ctx, f.userID, CreateRequest{
		HotelID: f.hotelID, RoomID: f.roomID,
		CheckIn: "2028-02-01", CheckOut: "2028-02-04", Guests: 2,
		GuestName: "Daw Su", GuestPhone: "0912345678",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if b.Status != StatusConfirmed {
		t.Fatalf("pay-at-property booking must be born confirmed, got %q", b.Status)
	}
	if b.ConfirmedAt == nil {
		t.Fatalf("born-confirmed booking must carry confirmed_at")
	}
	if b.PaymentMethod != PaymentMethodPayAtProperty {
		t.Fatalf("payment_method must default to pay_at_property, got %q", b.PaymentMethod)
	}
	if b.UnitNo != 1 {
		t.Fatalf("first unit must be 1, got %d", b.UnitNo)
	}
	if b.Nights != 3 || b.TotalMMK != b.PriceMMK*3 {
		t.Fatalf("nights/total wrong: nights=%d total=%d rate=%d", b.Nights, b.TotalMMK, b.PriceMMK)
	}

	// 2) Persisted row round-trips all pay-at-property fields.
	got, err := svc.GetByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusConfirmed || got.PaymentMethod != PaymentMethodPayAtProperty ||
		got.ConfirmedAt == nil || got.GuestName != "Daw Su" || got.GuestPhone != "0912345678" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	// 3) Explicit pay_at_property also confirms instantly.
	b2, err := svc.Create(ctx, f.userID, CreateRequest{
		HotelID: f.hotelID, RoomID: f.roomID,
		CheckIn: "2028-02-01", CheckOut: "2028-02-02", Guests: 1,
		PaymentMethod: PaymentMethodPayAtProperty,
	})
	if err != nil {
		t.Fatalf("create explicit method: %v", err)
	}
	if b2.Status != StatusConfirmed || b2.UnitNo != 2 {
		t.Fatalf("explicit pay_at_property: want confirmed/unit 2, got %q/unit %d", b2.Status, b2.UnitNo)
	}

	// 4) Unsupported payment method → rejected, and NO row is written.
	var before, after int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM bookings WHERE user_id = $1`, f.userID).Scan(&before); err != nil {
		t.Fatalf("count: %v", err)
	}
	if _, err := svc.Create(ctx, f.userID, CreateRequest{
		HotelID: f.hotelID, RoomID: f.roomID,
		CheckIn: "2028-03-01", CheckOut: "2028-03-02", Guests: 1,
		PaymentMethod: "card",
	}); !errors.Is(err, ErrPaymentUnsupported) {
		t.Fatalf("card payment must be ErrPaymentUnsupported until a gateway exists, got: %v", err)
	}
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM bookings WHERE user_id = $1`, f.userID).Scan(&after); err != nil {
		t.Fatalf("count: %v", err)
	}
	if before != after {
		t.Fatalf("unsupported payment method must not write a row (%d → %d)", before, after)
	}

	// 5) The mobile "Pending" tab filter (upcoming) returns the confirmed stay.
	upcoming, err := svc.ListByUser(ctx, f.userID, StatusPending)
	if err != nil {
		t.Fatalf("list upcoming: %v", err)
	}
	if len(upcoming) != 2 {
		t.Fatalf("upcoming tab must return both confirmed stays, got %d", len(upcoming))
	}

	// 6) Owner can reschedule a CONFIRMED booking (no online money to move).
	moved, err := svc.Reschedule(ctx, f.userID, b2.ID, "2028-02-05", "2028-02-07")
	if err != nil {
		t.Fatalf("reschedule confirmed booking: %v", err)
	}
	if moved.CheckIn != "2028-02-05" || moved.CheckOut != "2028-02-07" {
		t.Fatalf("reschedule dates not applied: %+v", moved)
	}

	// 7) Owner can CANCEL a confirmed booking; it then leaves the upcoming tab.
	if _, err := svc.Cancel(ctx, f.userID, b.ID); err != nil {
		t.Fatalf("cancel confirmed booking: %v", err)
	}
	upcoming, err = svc.ListByUser(ctx, f.userID, StatusPending)
	if err != nil {
		t.Fatalf("list after cancel: %v", err)
	}
	if len(upcoming) != 1 || upcoming[0].ID != b2.ID {
		t.Fatalf("after cancel the upcoming tab must hold only the rescheduled stay, got %+v", upcoming)
	}

	// 8) A completed stay is terminal for the owner (no cancel/reschedule).
	if _, err := tx.Exec(ctx, `UPDATE bookings SET status='complete' WHERE id=$1`, b2.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := svc.Cancel(ctx, f.userID, b2.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("complete stay must reject cancel with ErrNotPending, got: %v", err)
	}
	if _, err := svc.Reschedule(ctx, f.userID, b2.ID, "2028-04-01", "2028-04-02"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("complete stay must reject reschedule with ErrNotPending, got: %v", err)
	}
}

// TestUnitScopedConstraintShape asserts the constraint definition itself
// — a passing insert test alone does not prove the shape is right.
func TestUnitScopedConstraintShape(t *testing.T) {
	pool := requireAvailDB(t)
	ctx := context.Background()

	var conDef string
	if err := pool.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conname = 'no_double_booking' AND conrelid = 'bookings'::regclass`,
	).Scan(&conDef); err != nil {
		t.Fatalf("no_double_booking missing: %v", err)
	}
	for _, want := range []string{"room_id", "unit_no", "daterange", `'[)'`, "pending", "confirmed"} {
		if !strings.Contains(conDef, want) {
			t.Fatalf("constraint must mention %q: got %s", want, conDef)
		}
	}
	// The column must exist and be NOT NULL (legacy rows default to 1).
	var notNull bool
	if err := pool.QueryRow(ctx, `
		SELECT attnotnull FROM pg_attribute
		WHERE attrelid = 'bookings'::regclass AND attname = 'unit_no'`,
	).Scan(&notNull); err != nil || !notNull {
		t.Fatalf("bookings.unit_no must be NOT NULL (err=%v)", err)
	}
}
