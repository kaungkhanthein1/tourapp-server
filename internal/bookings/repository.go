package bookings

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dbtx is satisfied by *pgxpool.Pool and pgx.Tx so the unit-assignment
// transactions can be exercised inside rollback test fixtures.
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// beginner is satisfied by *pgxpool.Pool only — the test path hands a
// live tx in and never begins a nested one.
type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Repository is the data-access boundary for bookings.
type Repository interface {
	Create(ctx context.Context, b *Booking) error
	GetByID(ctx context.Context, id int64) (*Booking, error)
	// ListByUser returns the user's bookings; with no status filters it
	// returns everything, with statuses it matches any of them (IN).
	ListByUser(ctx context.Context, userID int64, statuses ...string) ([]Booking, error)
	UpdateStatus(ctx context.Context, id int64, status string) error
	Reschedule(ctx context.Context, id int64, checkIn, checkOut string) error
	// RoomPricing returns room name, price, hotel name/place/image for booking creation.
	RoomPricing(ctx context.Context, hotelID, roomID int64) (*RoomPricing, error)
	// RoomAvailability returns per-room real-time availability for a hotel
	// over [checkIn, checkOut) — the engine behind GET /hotels/:id/availability.
	RoomAvailability(ctx context.Context, hotelID int64, checkIn, checkOut string) ([]RoomAvailability, error)
	// HotelExists reports whether the hotel row exists (availability 404s).
	HotelExists(ctx context.Context, hotelID int64) (bool, error)
}

// RoomPricing is the denormalized snapshot used at booking time.
type RoomPricing struct {
	RoomName  string
	PriceMMK  int64
	HotelName string
	Place     string
	ImageURL  string
}

// PostgresRepository writes Neon PostgreSQL.
type PostgresRepository struct {
	db dbtx
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{db: pool}
}

var _ Repository = (*PostgresRepository)(nil)

// withTx runs fn inside a transaction when the repository owns a pool,
// or directly on the caller's tx in tests (the outer fixture rollback
// provides isolation).
func (r *PostgresRepository) withTx(ctx context.Context, fn func(dbtx) error) error {
	if b, ok := r.db.(beginner); ok {
		tx, err := b.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	return fn(r.db)
}

// lowestFreeUnit picks the lowest unit_no in [1..totalUnits] with no
// active (pending/confirmed) hold overlapping [checkIn, checkOut).
// Returns pgx.ErrNoRows when every unit is taken.
func lowestFreeUnit(ctx context.Context, q dbtx, roomID int64, totalUnits int, checkIn, checkOut string) (int, error) {
	var unit int
	err := q.QueryRow(ctx, `
		SELECT u
		FROM generate_series(1, $2) u
		WHERE NOT EXISTS (
			SELECT 1 FROM bookings b
			WHERE b.room_id = $1
			  AND b.unit_no = u
			  AND b.status IN ('pending','confirmed')
			  AND b.check_in < $3::date
			  AND b.check_out > $4::date
		)
		ORDER BY u
		LIMIT 1`, roomID, totalUnits, checkOut, checkIn).Scan(&unit)
	return unit, err
}

// Create books a unit of a room inside one serialized transaction:
//
//  1. lock the room row (FOR UPDATE) — concurrent bookers of the same
//     room serialize here, so the unit scan below is race-free;
//  2. reject when the room is stop-sold (is_active = false);
//  3. pick the lowest free unit for the window;
//  4. insert the booking holding that unit, with the status and
//     confirmed_at the service decided (pay-at-property: born confirmed).
//
// The unit-scoped no_double_booking EXCLUDE constraint stays as the hard
// backstop: even if this logic were bypassed, Postgres rejects any unit
// double-hold (SQLSTATE 23P01).
func (r *PostgresRepository) Create(ctx context.Context, b *Booking) error {
	return r.withTx(ctx, func(q dbtx) error {
		var totalUnits int
		var isActive bool
		err := q.QueryRow(ctx, `
			SELECT total_units, is_active
			FROM hotel_rooms
			WHERE hotel_id = $1 AND id = $2
			FOR UPDATE`, b.HotelID, b.RoomID).Scan(&totalUnits, &isActive)
		if err != nil {
			return err
		}
		if !isActive {
			return ErrRoomInactive
		}
		if totalUnits < 1 {
			totalUnits = 1
		}
		unit, err := lowestFreeUnit(ctx, q, b.RoomID, totalUnits, b.CheckIn, b.CheckOut)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotAvailable
			}
			return fmt.Errorf("pick unit: %w", err)
		}
		b.UnitNo = unit
		return q.QueryRow(ctx, `
			INSERT INTO bookings (user_id, hotel_id, hotel_name, room_name, place, image_url,
			                      price_mmk, check_in, check_out, guests, status,
			                      room_id, unit_no, nights, total_mmk, guest_name, guest_phone,
			                      payment_method, confirmed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8::date,$9::date,$10,$11,$12,$13,$14,$15,NULLIF($16,''),NULLIF($17,''),
			        $18, CASE WHEN $11 IN ('confirmed','complete') THEN COALESCE($19::timestamptz, now()) END)
			RETURNING id, created_at`,
			b.UserID, b.HotelID, b.HotelName, b.RoomName, b.Place, b.ImageURL,
			b.PriceMMK, b.CheckIn, b.CheckOut, b.Guests, b.Status,
			b.RoomID, b.UnitNo, b.Nights, b.TotalMMK, b.GuestName, b.GuestPhone,
			b.PaymentMethod, b.ConfirmedAt,
		).Scan(&b.ID, &b.CreatedAt)
	})
}

func (r *PostgresRepository) GetByID(ctx context.Context, id int64) (*Booking, error) {
	b, err := scanBooking(r.db.QueryRow(ctx, bookingSelect+" WHERE id = $1", id))
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (r *PostgresRepository) ListByUser(ctx context.Context, userID int64, statuses ...string) ([]Booking, error) {
	query := bookingSelect + " WHERE user_id = $1"
	args := []any{userID}
	if len(statuses) > 0 {
		// status = ANY(array) — one placeholder per status, e.g. the
		// "upcoming" tab passes ('pending','confirmed').
		placeholders := make([]string, len(statuses))
		for i, st := range statuses {
			args = append(args, st)
			placeholders[i] = fmt.Sprintf("$%d", len(args))
		}
		query += " AND status = ANY(ARRAY[" + strings.Join(placeholders, ",") + "])"
	}
	query += " ORDER BY created_at DESC, id DESC"

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Booking
	for rows.Next() {
		b, err := scanBooking(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *b)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) UpdateStatus(ctx context.Context, id int64, status string) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE bookings
		SET status      = $2,
		    confirmed_at = CASE WHEN $2 = 'confirmed' THEN now() ELSE confirmed_at END,
		    cancelled_at = CASE WHEN $2 = 'cancelled' THEN now() ELSE cancelled_at END
		WHERE id = $1`, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// Reschedule moves an upcoming (pending or confirmed) booking to new
// dates inside the same serialized protocol as Create: lock the room,
// verify stop-sell, and re-pick a free unit for the NEW window (the
// booking's old unit may be taken by someone else at the target dates).
// The exclusion constraint re-fires on the UPDATE as the backstop.
func (r *PostgresRepository) Reschedule(ctx context.Context, id int64, checkIn, checkOut string) error {
	return r.withTx(ctx, func(q dbtx) error {
		var hotelID, roomID int64
		var totalUnits int
		var isActive bool
		err := q.QueryRow(ctx, `
			SELECT b.hotel_id, b.room_id, r.total_units, r.is_active
			FROM bookings b
			JOIN hotel_rooms r ON r.id = b.room_id
			WHERE b.id = $1 AND b.status IN ('pending','confirmed')
			FOR UPDATE OF b, r`, id).Scan(&hotelID, &roomID, &totalUnits, &isActive)
		if err != nil {
			return err
		}
		if !isActive {
			return ErrRoomInactive
		}
		if totalUnits < 1 {
			totalUnits = 1
		}
		// The booking keeps its current unit when that unit is still free
		// at the target dates; otherwise the lowest free unit wins.
		var currentUnit int
		if err := q.QueryRow(ctx,
			`SELECT unit_no FROM bookings WHERE id = $1`, id).Scan(&currentUnit); err != nil {
			return err
		}
		unit := currentUnit
		var taken bool
		if err := q.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM bookings b
				WHERE b.room_id = $1 AND b.unit_no = $2 AND b.id <> $3
				  AND b.status IN ('pending','confirmed')
				  AND b.check_in < $4::date AND b.check_out > $5::date
			)`, roomID, currentUnit, id, checkOut, checkIn).Scan(&taken); err != nil {
			return err
		}
		if taken {
			unit, err = lowestFreeUnit(ctx, q, roomID, totalUnits, checkIn, checkOut)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotAvailable
				}
				return fmt.Errorf("pick unit: %w", err)
			}
		}
		tag, err := q.Exec(ctx, `
			UPDATE bookings
			SET check_in  = $2::date,
			    check_out = $3::date,
			    unit_no   = $4,
			    nights    = GREATEST($3::date - $2::date, 1),
			    total_mmk = price_mmk * GREATEST($3::date - $2::date, 1)
			WHERE id = $1`, id, checkIn, checkOut, unit)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return nil
	})
}

// RoomAvailability returns every room of a hotel with its real-time
// state for [checkIn, checkOut): the peak number of active holds
// covering any single night of the window (a room is bookable only when
// EVERY night has a free unit), the free-unit count, and the stop-sell
// flag. Pure read — no locks, served from the partial index.
func (r *PostgresRepository) RoomAvailability(ctx context.Context, hotelID int64, checkIn, checkOut string) ([]RoomAvailability, error) {
	rows, err := r.db.Query(ctx, `
		SELECT r.id, r.name, r.price_mmk, r.capacity, r.image_url, COALESCE(r.description,''),
		       r.total_units, r.is_active,
		       COALESCE(pk.peak, 0)
		FROM hotel_rooms r
		LEFT JOIN LATERAL (
			SELECT MAX(c.n) AS peak
			FROM (
				SELECT count(b.id) AS n
				FROM generate_series($2::date, $3::date - 1, '1 day') d(day)
				LEFT JOIN bookings b
				  ON b.room_id = r.id
				 AND b.status IN ('pending','confirmed')
				 AND d.day >= b.check_in AND d.day < b.check_out
				GROUP BY d.day
			) c
		) pk ON TRUE
		WHERE r.hotel_id = $1
		ORDER BY r.id`, hotelID, checkIn, checkOut)
	if err != nil {
		return nil, fmt.Errorf("room availability: %w", err)
	}
	defer rows.Close()

	var out []RoomAvailability
	for rows.Next() {
		var a RoomAvailability
		if err := rows.Scan(&a.RoomID, &a.Name, &a.PriceMMK, &a.Capacity, &a.ImageURL,
			&a.Description, &a.TotalUnits, &a.IsActive, &a.UnitsBooked); err != nil {
			return nil, fmt.Errorf("scan availability: %w", err)
		}
		a.UnitsFree = a.TotalUnits - a.UnitsBooked
		if a.UnitsFree < 0 {
			a.UnitsFree = 0
		}
		a.Bookable = a.IsActive && a.UnitsFree > 0
		out = append(out, a)
	}
	return out, rows.Err()
}

// HotelExists reports whether a hotel row is present.
func (r *PostgresRepository) HotelExists(ctx context.Context, hotelID int64) (bool, error) {
	var ok bool
	if err := r.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM hotels WHERE id = $1)`, hotelID).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

// IsExclusionViolation reports whether err is the no_double_booking
// EXCLUDE constraint rejecting a write (Postgres SQLSTATE 23P01).
func IsExclusionViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}

func (r *PostgresRepository) RoomPricing(ctx context.Context, hotelID, roomID int64) (*RoomPricing, error) {
	var p RoomPricing
	err := r.db.QueryRow(ctx, `
		SELECT r.name, r.price_mmk, h.name, h.place, h.image_url
		FROM hotel_rooms r
		JOIN hotels h ON h.id = r.hotel_id
		WHERE r.hotel_id = $1 AND r.id = $2`,
		hotelID, roomID,
	).Scan(&p.RoomName, &p.PriceMMK, &p.HotelName, &p.Place, &p.ImageURL)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// bookingSelect: room_id is COALESCE'd to 0 for legacy rows whose room
// was deleted before the Phase 0 backfill — they stay unconstrained by
// the exclusion rule (NULL room) but must still scan into the int64 field.
// payment_method defaults to pay_at_property for pre-Phase-3 rows.
const bookingSelect = `
	SELECT id, user_id, hotel_id, COALESCE(room_id, 0), unit_no, hotel_name, room_name, place, image_url,
	       price_mmk, nights, total_mmk, COALESCE(guest_name,''), COALESCE(guest_phone,''),
	       COALESCE(payment_method,'pay_at_property'),
	       check_in::text, check_out::text, guests, status, confirmed_at, cancelled_at, created_at
	FROM bookings`

type scanner interface {
	Scan(dest ...any) error
}

func scanBooking(row scanner) (*Booking, error) {
	var b Booking
	if err := row.Scan(&b.ID, &b.UserID, &b.HotelID, &b.RoomID, &b.UnitNo, &b.HotelName, &b.RoomName, &b.Place,
		&b.ImageURL, &b.PriceMMK, &b.Nights, &b.TotalMMK, &b.GuestName, &b.GuestPhone, &b.PaymentMethod,
		&b.CheckIn, &b.CheckOut, &b.Guests, &b.Status, &b.ConfirmedAt, &b.CancelledAt,
		&b.CreatedAt); err != nil {
		return nil, err
	}
	return &b, nil
}
