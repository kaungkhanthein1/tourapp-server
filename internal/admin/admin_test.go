package admin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"travel-app-backend/internal/bookings"
	"travel-app-backend/internal/database"
	"travel-app-backend/internal/pkg/pagination"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
)

// Phase 1 extranet integration tests against the real database
// (DATABASE_URL from ../../.env). Every scenario runs inside one
// transaction that is always rolled back — the dev DB stays clean.
// All tests skip when DATABASE_URL is unset.

var (
	adminOnce sync.Once
	adminPool *pgxpool.Pool
	adminErr  error
)

func requireAdminDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	_ = godotenv.Load("../../.env")
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping admin extranet tests")
	}
	adminOnce.Do(func() {
		ctx := context.Background()
		adminPool, adminErr = database.NewPool(ctx)
		if adminErr == nil {
			adminErr = database.Migrate(ctx, adminPool)
		}
	})
	if adminErr != nil {
		t.Fatalf("prepare database: %v", adminErr)
	}
	return adminPool
}

// fx is a rollback-only fixture: one admin, one guest, one hotel, one
// room — everything an admin scenario needs.
type fx struct {
	adminID int64
	guestID int64
	hotelID int64
	roomID  int64
	tag     string
}

func newFx(t *testing.T, ctx context.Context, tx pgx.Tx) fx {
	t.Helper()
	f := fx{tag: fmt.Sprintf("admin-%d", time.Now().UnixNano())}
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (firebase_uid, name, email, role)
		VALUES ($1, 'admin-test', $2, 'admin') RETURNING id`,
		"fb:"+f.tag, f.tag+"@admin.local").Scan(&f.adminID); err != nil {
		t.Fatalf("fixture admin: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO users (firebase_uid, name, email)
		VALUES ($1, 'guest-test', $2) RETURNING id`,
		"fb:g:"+f.tag, "g-"+f.tag+"@admin.local").Scan(&f.guestID); err != nil {
		t.Fatalf("fixture guest: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO hotels (name, place, country, image_url)
		VALUES ($1, 'Adminville', 'TT', '') RETURNING id`,
		"ADMIN TEST "+f.tag).Scan(&f.hotelID); err != nil {
		t.Fatalf("fixture hotel: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO hotel_rooms (hotel_id, name, price_mmk, image_url, total_units)
		VALUES ($1, 'Test Room', 100000, '', 1) RETURNING id`, f.hotelID).Scan(&f.roomID); err != nil {
		t.Fatalf("fixture room: %v", err)
	}
	return f
}

func (f fx) insertBooking(ctx context.Context, t *testing.T, tx pgx.Tx, in, out, status string) int64 {
	t.Helper()
	var id int64
	err := tx.QueryRow(ctx, `
		INSERT INTO bookings (user_id, hotel_id, room_id, hotel_name, room_name, place, image_url,
		                      price_mmk, nights, total_mmk, check_in, check_out, guests, status)
		VALUES ($1,$2,$3,'ADMIN TEST','Test Room','Adminville','',100000,
		        GREATEST($5::date - $4::date, 1),
		        100000 * GREATEST($5::date - $4::date, 1),
		        $4::date, $5::date, 1, $6)
		RETURNING id`,
		f.guestID, f.hotelID, f.roomID, in, out, status).Scan(&id)
	if err != nil {
		t.Fatalf("fixture booking: %v", err)
	}
	return id
}

// svc builds a Service whose repository runs on the fixture transaction.
func (f fx) svc(tx pgx.Tx) *Service {
	return NewService(NewRepository(tx))
}

func beginTx(t *testing.T, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}) (pgx.Tx, context.Context) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	return tx, ctx
}

func auditCount(ctx context.Context, t *testing.T, tx pgx.Tx, action string, entityID int64) int64 {
	t.Helper()
	var n int64
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_logs WHERE action = $1 AND entity_id = $2`,
		action, entityID).Scan(&n)
	if err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

// ── Status machine ────────────────────────────────────────────────────

func TestAdminBookingStatusMachine(t *testing.T) {
	pool := requireAdminDB(t)
	tx, ctx := beginTx(t, pool)
	f := newFx(t, ctx, tx)
	svc := f.svc(tx)

	id := f.insertBooking(ctx, t, tx, "2027-03-01", "2027-03-03", bookings.StatusPending)

	// pending → confirmed: allowed, stamps confirmed_at, writes audit.
	b, err := svc.ChangeStatus(ctx, f.adminID, id, UpdateStatusRequest{Status: "confirmed", Reason: "payment received"})
	if err != nil {
		t.Fatalf("pending→confirmed: %v", err)
	}
	if b.Status != "confirmed" {
		t.Fatalf("expected confirmed, got %s", b.Status)
	}
	if b.ConfirmedAt == nil {
		t.Fatal("confirmed_at must be stamped on confirmation")
	}
	if n := auditCount(ctx, t, tx, "booking.status_changed", id); n != 1 {
		t.Fatalf("expected 1 audit row, got %d", n)
	}

	// confirmed → complete: allowed (terminal).
	if _, err := svc.ChangeStatus(ctx, f.adminID, id, UpdateStatusRequest{Status: "complete"}); err != nil {
		t.Fatalf("confirmed→complete: %v", err)
	}

	// complete → anything: terminal, rejected.
	if _, err := svc.ChangeStatus(ctx, f.adminID, id, UpdateStatusRequest{Status: "cancelled"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("complete→cancelled must be rejected, got: %v", err)
	}

	// pending → complete: skipping a stage, rejected.
	id2 := f.insertBooking(ctx, t, tx, "2027-04-01", "2027-04-03", bookings.StatusPending)
	if _, err := svc.ChangeStatus(ctx, f.adminID, id2, UpdateStatusRequest{Status: "complete"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("pending→complete must be rejected, got: %v", err)
	}

	// pending → pending: no-op transition, rejected.
	if _, err := svc.ChangeStatus(ctx, f.adminID, id2, UpdateStatusRequest{Status: "pending"}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("pending→pending must be rejected, got: %v", err)
	}

	// Bogus status: 400-class domain error.
	if _, err := svc.ChangeStatus(ctx, f.adminID, id2, UpdateStatusRequest{Status: "archived"}); !errors.Is(err, ErrInvalidStatus) {
		t.Fatalf("bogus status must be ErrInvalidStatus, got: %v", err)
	}

	// cancelling frees inventory: overlap that was blocked while pending
	// must now be insertable (the exclusion constraint drops cancelled).
	id3 := f.insertBooking(ctx, t, tx, "2027-05-01", "2027-05-03", bookings.StatusPending)
	if _, err := svc.ChangeStatus(ctx, f.adminID, id3, UpdateStatusRequest{Status: "cancelled", Reason: "guest request"}); err != nil {
		t.Fatalf("pending→cancelled: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO bookings (user_id, hotel_id, room_id, hotel_name, room_name, place, image_url,
		                      price_mmk, nights, total_mmk, check_in, check_out, guests, status)
		VALUES ($1,$2,$3,'ADMIN TEST','Test Room','Adminville','',100000,2,200000,'2027-05-01','2027-05-03',1,'pending')`,
		f.guestID, f.hotelID, f.roomID); err != nil {
		t.Fatalf("cancelled booking must free its range: %v", err)
	}
}

// ── Booking edit ──────────────────────────────────────────────────────

func TestAdminEditBooking(t *testing.T) {
	pool := requireAdminDB(t)
	tx, ctx := beginTx(t, pool)
	f := newFx(t, ctx, tx)
	svc := f.svc(tx)

	holder := f.insertBooking(ctx, t, tx, "2027-06-01", "2027-06-05", bookings.StatusPending) // holds Jun 1-5
	target := f.insertBooking(ctx, t, tx, "2027-06-10", "2027-06-12", bookings.StatusPending)

	// Overlapping edit must hit the no_double_booking constraint →
	// bookings.ErrNotAvailable (the handler maps it to 409).
	// Postgres aborts the tx on 23P01, so run under a savepoint.
	if _, err := tx.Exec(ctx, `SAVEPOINT overlap_edit`); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	in, out := "2027-06-02", "2027-06-04"
	_, err := svc.EditBooking(ctx, f.adminID, target, UpdateBookingRequest{CheckIn: &in, CheckOut: &out})
	if !errors.Is(err, bookings.ErrNotAvailable) {
		t.Fatalf("overlapping edit must return ErrNotAvailable, got: %v", err)
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT overlap_edit`); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}
	_ = holder

	// Legal edit: move to a free window + patch guests; nights/total recompute.
	newIn, newOut := "2027-06-05", "2027-06-08" // back-to-back after holder checkout
	name, phone := "Daw", "+95912345"
	b, err := svc.EditBooking(ctx, f.adminID, target, UpdateBookingRequest{
		CheckIn: &newIn, CheckOut: &newOut, GuestName: &name, GuestPhone: &phone,
	})
	if err != nil {
		t.Fatalf("legal edit: %v", err)
	}
	if b.Nights != 3 || b.TotalMMK != 300000 {
		t.Fatalf("edit must recompute nights/total (3/300000), got %d/%d", b.Nights, b.TotalMMK)
	}
	if b.GuestName != "Daw" || b.GuestPhone != "+95912345" {
		t.Fatalf("guest fields not saved: %+v", b)
	}
	if n := auditCount(ctx, t, tx, "booking.updated", target); n != 1 {
		t.Fatalf("expected 1 booking.updated audit row, got %d", n)
	}

	// Backwards dates: ErrInvalidDates.
	bad := "2027-01-01"
	if _, err := svc.EditBooking(ctx, f.adminID, target, UpdateBookingRequest{CheckOut: &bad}); !errors.Is(err, ErrInvalidDates) {
		t.Fatalf("backwards dates must be ErrInvalidDates, got: %v", err)
	}

	// Finished bookings are read-only.
	done := f.insertBooking(ctx, t, tx, "2027-07-01", "2027-07-03", bookings.StatusComplete)
	if _, err := svc.EditBooking(ctx, f.adminID, done, UpdateBookingRequest{GuestName: &name}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("complete booking edit must be rejected, got: %v", err)
	}
}

// ── Delete guards ─────────────────────────────────────────────────────

func TestAdminDeleteGuards(t *testing.T) {
	pool := requireAdminDB(t)
	tx, ctx := beginTx(t, pool)
	f := newFx(t, ctx, tx)
	svc := f.svc(tx)

	// Booking references the room → deletion refused (deactivate instead).
	// Postgres aborts the tx on the RESTRICT violation, so wrap in a
	// savepoint like the exclusion tests.
	f.insertBooking(ctx, t, tx, "2027-08-01", "2027-08-03", bookings.StatusComplete)
	if _, err := tx.Exec(ctx, `SAVEPOINT room_delete`); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	if err := svc.DeleteRoom(ctx, f.adminID, f.hotelID, f.roomID); !errors.Is(err, ErrRoomHasBookings) {
		t.Fatalf("room with bookings must be undeletable, got: %v", err)
	}
	if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT room_delete`); err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}

	// Same for the hotel (bookings.hotel_id is CASCADE — the guard is
	// the only thing preventing silent history destruction).
	if err := svc.DeleteHotel(ctx, f.adminID, f.hotelID); !errors.Is(err, ErrHotelHasBookings) {
		t.Fatalf("hotel with bookings must be undeletable, got: %v", err)
	}

	// A fresh room (no bookings) deletes fine.
	var roomID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO hotel_rooms (hotel_id, name, price_mmk, image_url) VALUES ($1,'Fresh Room',1,'') RETURNING id`,
		f.hotelID).Scan(&roomID); err != nil {
		t.Fatalf("fresh room: %v", err)
	}
	if err := svc.DeleteRoom(ctx, f.adminID, f.hotelID, roomID); err != nil {
		t.Fatalf("bookings-free room must delete: %v", err)
	}
	if n := auditCount(ctx, t, tx, "room.deleted", roomID); n != 1 {
		t.Fatalf("expected 1 room.deleted audit row, got %d", n)
	}
}

// ── Hotel/room CRUD + inventory ───────────────────────────────────────

func TestAdminHotelRoomCRUD(t *testing.T) {
	pool := requireAdminDB(t)
	tx, ctx := beginTx(t, pool)
	f := newFx(t, ctx, tx)
	svc := f.svc(tx)

	// Create.
	h, err := svc.CreateHotel(ctx, f.adminID, CreateHotelRequest{
		Name: "CRUD Hotel", Place: "Yangon", Country: "MM", Rating: 4.5,
	})
	if err != nil {
		t.Fatalf("create hotel: %v", err)
	}
	if h.Source != "admin" {
		t.Fatalf("admin-created hotels must be marked source=admin, got %q", h.Source)
	}

	// Patch (supplied fields only).
	up := "CRUD Hotel Renamed"
	h2, err := svc.UpdateHotel(ctx, f.adminID, h.ID, UpdateHotelRequest{Name: &up})
	if err != nil {
		t.Fatalf("update hotel: %v", err)
	}
	if h2.Name != "CRUD Hotel Renamed" || h2.Place != "Yangon" {
		t.Fatalf("partial update must keep untouched fields: %+v", h2)
	}

	// Room with inventory.
	rm, err := svc.CreateRoom(ctx, f.adminID, h.ID, RoomRequest{
		Name: "Suite", PriceMMK: 50000, Capacity: 3, TotalUnits: 4,
	})
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	if rm.TotalUnits != 4 || !rm.IsActive {
		t.Fatalf("room inventory defaults wrong: units=%d active=%v", rm.TotalUnits, rm.IsActive)
	}

	// Stop-sell via inventory endpoint semantics.
	active := false
	rm2, err := svc.SetRoomInventory(ctx, f.adminID, h.ID, rm.ID, InventoryRequest{TotalUnits: 6, IsActive: &active})
	if err != nil {
		t.Fatalf("set inventory: %v", err)
	}
	if rm2.TotalUnits != 6 || rm2.IsActive {
		t.Fatalf("inventory not applied: units=%d active=%v", rm2.TotalUnits, rm2.IsActive)
	}
	if n := auditCount(ctx, t, tx, "room.inventory_changed", rm.ID); n != 1 {
		t.Fatalf("expected 1 inventory audit row, got %d", n)
	}

	// Counts visible in the list view.
	hotels, total, err := svc.ListHotels(ctx, "CRUD", pagination.Page{Limit: 10, Offset: 0})
	if err != nil {
		t.Fatalf("list hotels: %v", err)
	}
	if total < 1 || len(hotels) == 0 {
		t.Fatalf("created hotel must appear in the filtered list (total=%d)", total)
	}

	// 404 paths.
	if _, err := svc.GetHotel(ctx, 999999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing hotel must be ErrNotFound, got: %v", err)
	}
	if err := svc.DeleteRoom(ctx, f.adminID, h.ID, 999999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing room must be ErrNotFound, got: %v", err)
	}
}

// ── Stats & occupancy ─────────────────────────────────────────────────

func TestAdminStatsOccupancy(t *testing.T) {
	pool := requireAdminDB(t)
	tx, ctx := beginTx(t, pool)
	f := newFx(t, ctx, tx)
	svc := f.svc(tx)

	// Two room rows (1 unit each — the exclusion constraint is per room
	// row, so a room row is one sellable unit): overlapping bookings go
	// on different rooms. A covers Sep 1-2 nights, B covers Sep 2-3.
	f.insertBooking(ctx, t, tx, "2027-09-01", "2027-09-03", bookings.StatusPending) // covers Sep 1,2
	var roomB int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO hotel_rooms (hotel_id, name, price_mmk, image_url, total_units)
		VALUES ($1, 'Test Room B', 100000, '', 1) RETURNING id`, f.hotelID).Scan(&roomB); err != nil {
		t.Fatalf("second room: %v", err)
	}
	var b2ID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO bookings (user_id, hotel_id, room_id, hotel_name, room_name, place, image_url,
		                      price_mmk, nights, total_mmk, check_in, check_out, guests, status)
		VALUES ($1,$2,$3,'ADMIN TEST','Test Room B','Adminville','',100000,2,200000,'2027-09-02','2027-09-04',1,'confirmed')
		RETURNING id`, f.guestID, f.hotelID, roomB).Scan(&b2ID); err != nil {
		t.Fatalf("second booking: %v", err)
	}

	days, err := svc.Occupancy(ctx, "2027-09-01", "2027-09-05", f.hotelID)
	if err != nil {
		t.Fatalf("occupancy: %v", err)
	}
	if len(days) != 4 {
		t.Fatalf("expected 4 days, got %d", len(days))
	}
	want := []struct {
		date   string
		booked int64
		pct    float64
	}{
		{"2027-09-01", 1, 50},
		{"2027-09-02", 2, 100},
		{"2027-09-03", 1, 50},
		{"2027-09-04", 0, 0},
	}
	for i, w := range want {
		if days[i].Date != w.date || days[i].BookedNights != w.booked || days[i].OccupancyPct != w.pct {
			t.Fatalf("day %d: want %s %d/%%%.0f, got %s %d/%.0f",
				i, w.date, w.booked, w.pct, days[i].Date, days[i].BookedNights, days[i].OccupancyPct)
		}
		if days[i].UnitsForSale != 2 {
			t.Fatalf("day %d: expected units_for_sale=2, got %d", i, days[i].UnitsForSale)
		}
	}

	// Date validation.
	if _, err := svc.Occupancy(ctx, "2027-09-05", "2027-09-01", 0); err == nil {
		t.Fatal("reversed range must be rejected")
	}
	if _, err := svc.Occupancy(ctx, "not-a-date", "2027-09-01", 0); err == nil {
		t.Fatal("malformed date must be rejected")
	}

	// Overview: our fixture contributes 1 pending + 1 confirmed.
	stats, err := svc.StatsOverview(ctx)
	if err != nil {
		t.Fatalf("overview: %v", err)
	}
	if stats.PendingBookings < 1 || stats.ConfirmedBookings < 1 {
		t.Fatalf("overview must see the fixture bookings: %+v", stats)
	}
	if stats.ActiveHolds != stats.PendingBookings+stats.ConfirmedBookings {
		t.Fatalf("active holds must equal pending+confirmed: %+v", stats)
	}
}

// ── Users & audit ─────────────────────────────────────────────────────

func TestAdminUsersAudit(t *testing.T) {
	pool := requireAdminDB(t)
	tx, ctx := beginTx(t, pool)
	f := newFx(t, ctx, tx)
	svc := f.svc(tx)

	// Promote the guest.
	u, err := svc.SetUserRole(ctx, f.adminID, f.guestID, SetRoleRequest{Role: "ADMIN"})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if u.Role != "admin" {
		t.Fatalf("role must normalize to lowercase admin, got %q", u.Role)
	}

	// Audit records before/after + email.
	var details map[string]any
	var email string
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(details,'{}'::jsonb), COALESCE(u.email,'')
		FROM admin_audit_logs a LEFT JOIN users u ON u.id = a.admin_id
		WHERE a.action = 'user.role_changed' AND a.entity_id = $1`, f.guestID,
	).Scan(&details, &email); err != nil {
		t.Fatalf("role audit row: %v", err)
	}
	if details["before"] != "user" || details["after"] != "admin" {
		t.Fatalf("audit before/after wrong: %+v", details)
	}

	// Demote back (revocation path).
	u, err = svc.SetUserRole(ctx, f.adminID, f.guestID, SetRoleRequest{Role: "user"})
	if err != nil || u.Role != "user" {
		t.Fatalf("demote: err=%v role=%q", err, u.Role)
	}

	// Invalid roles rejected.
	for _, bad := range []string{"root", "", "Admin!", "superuser"} {
		if _, err := svc.SetUserRole(ctx, f.adminID, f.guestID, SetRoleRequest{Role: bad}); !errors.Is(err, ErrInvalidRole) {
			t.Fatalf("role %q must be ErrInvalidRole, got: %v", bad, err)
		}
	}

	// User list exposes role + booking counts and filters.
	users, _, err := svc.ListUsers(ctx, "admin.local", "", pagination.Page{Limit: 50, Offset: 0})
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	found := false
	for _, usr := range users {
		if usr.ID == f.adminID {
			found = true
			if usr.Role != "admin" {
				t.Fatalf("fixture admin must show role=admin, got %q", usr.Role)
			}
		}
	}
	if !found {
		t.Fatalf("fixture admin missing from user list (q=admin.local)")
	}

	// Audit trail lists newest-first with admin email attribution.
	logs, total, err := svc.ListAuditLogs(ctx, AuditFilter{EntityType: "user", Page: pagination.Page{Limit: 50, Offset: 0}})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if total < 2 || len(logs) < 2 {
		t.Fatalf("expected ≥2 user audit rows, got %d", total)
	}
	for _, l := range logs {
		if l.AdminID == nil || *l.AdminID != f.adminID {
			t.Fatalf("audit row must attribute the acting admin: %+v", l)
		}
	}
}
