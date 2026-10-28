package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"travel-app-backend/internal/pkg/pagination"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dbtx is satisfied by both *pgxpool.Pool and pgx.Tx so the repository
// can run inside test transactions (rollback fixtures).
type dbtx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Repository is the data-access boundary for the admin extranet.
type Repository struct {
	db dbtx
}

// NewRepository builds a Repository over the pool (production) or a tx (tests).
func NewRepository(db dbtx) *Repository { return &Repository{db: db} }

// NewPostgresRepository adapts the pool to the admin repository.
func NewPostgresRepository(pool *pgxpool.Pool) *Repository { return &Repository{db: pool} }

// ── Bookings ──────────────────────────────────────────────────────────

const adminBookingSelect = `
	SELECT b.id, b.user_id, b.hotel_id, COALESCE(b.room_id, 0), b.unit_no, b.hotel_name, b.room_name, b.place,
	       b.image_url, b.price_mmk, b.nights, b.total_mmk, COALESCE(b.guest_name,''), COALESCE(b.guest_phone,''),
	       COALESCE(b.payment_method,'pay_at_property'),
	       b.check_in::text, b.check_out::text, b.guests, b.status, b.confirmed_at, b.cancelled_at, b.created_at,
	       COALESCE(u.email,''), COALESCE(u.name,'')
	FROM bookings b
	LEFT JOIN users u ON u.id = b.user_id`

func scanAdminBooking(row pgx.Row) (*AdminBooking, error) {
	var b AdminBooking
	err := row.Scan(&b.ID, &b.UserID, &b.HotelID, &b.RoomID, &b.UnitNo, &b.HotelName, &b.RoomName, &b.Place,
		&b.ImageURL, &b.PriceMMK, &b.Nights, &b.TotalMMK, &b.GuestName, &b.GuestPhone, &b.PaymentMethod,
		&b.CheckIn, &b.CheckOut, &b.Guests, &b.Status, &b.ConfirmedAt, &b.CancelledAt, &b.CreatedAt,
		&b.UserEmail, &b.UserName)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// bookingWhere builds the shared filter for ListBookings + its count.
func bookingWhere(f BookingFilter) (string, []any) {
	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}
	if f.Status != "" {
		add("b.status = $%d", f.Status)
	}
	if f.HotelID > 0 {
		add("b.hotel_id = $%d", f.HotelID)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		args = append(args, q)
		n := len(args)
		conds = append(conds, fmt.Sprintf(`(b.hotel_name ILIKE '%%'||$%d||'%%' OR b.guest_name ILIKE '%%'||$%d||'%%'
		      OR b.guest_phone ILIKE '%%'||$%d||'%%' OR u.email ILIKE '%%'||$%d||'%%')`, n, n, n, n))
	}
	if f.From != "" {
		add("b.check_in >= $%d::date", f.From)
	}
	if f.To != "" {
		add("b.check_in < $%d::date", f.To)
	}
	if len(conds) == 0 {
		return "TRUE", args
	}
	return strings.Join(conds, " AND "), args
}

// ListBookings returns filtered bookings (newest first) + total count.
func (r *Repository) ListBookings(ctx context.Context, f BookingFilter) ([]AdminBooking, int64, error) {
	where, args := bookingWhere(f)

	var total int64
	if err := r.db.QueryRow(ctx, `
		SELECT count(*)
		FROM bookings b LEFT JOIN users u ON u.id = b.user_id
		WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count bookings: %w", err)
	}

	query := adminBookingSelect + ` WHERE ` + where +
		` ORDER BY b.created_at DESC, b.id DESC LIMIT $` + fmt.Sprint(len(args)+1) +
		` OFFSET $` + fmt.Sprint(len(args)+2)
	qargs := append(append([]any{}, args...), f.Page.Limit, f.Page.Offset)

	rows, err := r.db.Query(ctx, query, qargs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list bookings: %w", err)
	}
	defer rows.Close()

	var out []AdminBooking
	for rows.Next() {
		var b AdminBooking
		if err := rows.Scan(&b.ID, &b.UserID, &b.HotelID, &b.RoomID, &b.UnitNo, &b.HotelName, &b.RoomName, &b.Place,
			&b.ImageURL, &b.PriceMMK, &b.Nights, &b.TotalMMK, &b.GuestName, &b.GuestPhone, &b.PaymentMethod,
			&b.CheckIn, &b.CheckOut, &b.Guests, &b.Status, &b.ConfirmedAt, &b.CancelledAt, &b.CreatedAt,
			&b.UserEmail, &b.UserName); err != nil {
			return nil, 0, fmt.Errorf("scan booking: %w", err)
		}
		out = append(out, b)
	}
	return out, total, rows.Err()
}

// GetBooking returns one booking or pgx.ErrNoRows.
func (r *Repository) GetBooking(ctx context.Context, id int64) (*AdminBooking, error) {
	return scanAdminBooking(r.db.QueryRow(ctx, adminBookingSelect+` WHERE b.id = $1`, id))
}

// UpdateBookingStatus applies a transition, stamping confirmed_at or
// cancelled_at, and only when the status is still `expected` (the value
// the service read) — the guard makes the transition atomic even under
// concurrent admin edits.
func (r *Repository) UpdateBookingStatus(ctx context.Context, id int64, status, expected string) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE bookings
		SET status      = $2,
		    confirmed_at = CASE WHEN $2 = 'confirmed' THEN now() ELSE confirmed_at END,
		    cancelled_at = CASE WHEN $2 = 'cancelled' THEN now() ELSE cancelled_at END
		WHERE id = $1 AND status = $3`, id, status, expected)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// UpdateBooking patches editable columns and recomputes nights/total
// when dates move. The exclusion constraint re-fires on the UPDATE, so
// an overlapping target is rejected by Postgres (SQLSTATE 23P01).
func (r *Repository) UpdateBooking(ctx context.Context, id int64, req UpdateBookingRequest) error {
	var sets []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if req.GuestName != nil {
		sets = append(sets, "guest_name = NULLIF("+arg(*req.GuestName)+",'')")
	}
	if req.GuestPhone != nil {
		sets = append(sets, "guest_phone = NULLIF("+arg(*req.GuestPhone)+",'')")
	}
	if req.Guests != nil {
		sets = append(sets, "guests = "+arg(*req.Guests))
	}
	if req.CheckIn != nil || req.CheckOut != nil {
		// Merge the (validated) partial update over the stored pair, then
		// compute nights in Go — same rule as the bookings service.
		var curIn, curOut string
		if err := r.db.QueryRow(ctx,
			`SELECT check_in::text, check_out::text FROM bookings WHERE id = $1`, id,
		).Scan(&curIn, &curOut); err != nil {
			return err
		}
		in, out := curIn, curOut
		if req.CheckIn != nil {
			in = *req.CheckIn
		}
		if req.CheckOut != nil {
			out = *req.CheckOut
		}
		tIn, err1 := time.Parse("2006-01-02", in)
		tOut, err2 := time.Parse("2006-01-02", out)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("stored or new dates are not YYYY-MM-DD")
		}
		nights := int(tOut.Sub(tIn).Hours() / 24)
		sets = append(sets,
			"check_in = "+arg(in)+"::date",
			"check_out = "+arg(out)+"::date",
			fmt.Sprintf("nights = %d", nights),
			fmt.Sprintf("total_mmk = price_mmk * %d", nights))
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	tag, err := r.db.Exec(ctx,
		`UPDATE bookings SET `+strings.Join(sets, ", ")+` WHERE id = $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ── Hotels & rooms ────────────────────────────────────────────────────

const adminHotelSelect = `
	SELECT h.id, h.name, h.place, h.country, COALESCE(h.region,''), h.image_url,
	       COALESCE(h.gallery_urls, '{}'), h.rating, COALESCE(h.facilities, '{}'),
	       h.place_id, h.source, h.created_at,
	       (SELECT count(*) FROM hotel_rooms r WHERE r.hotel_id = h.id),
	       (SELECT count(*) FROM bookings b WHERE b.hotel_id = h.id AND b.status IN ('pending','confirmed'))
	FROM hotels h`

func scanAdminHotel(row pgx.Row) (*AdminHotel, error) {
	var h AdminHotel
	err := row.Scan(&h.ID, &h.Name, &h.Place, &h.Country, &h.Region, &h.ImageURL,
		&h.GalleryURLs, &h.Rating, &h.Facilities, &h.PlaceID, &h.Source, &h.CreatedAt,
		&h.RoomCount, &h.ActiveBookings)
	if err != nil {
		return nil, err
	}
	return &h, nil
}

// ListHotels returns hotels (optionally filtered by q) + total count.
func (r *Repository) ListHotels(ctx context.Context, q string, page pagination.Page) ([]AdminHotel, int64, error) {
	where, args := "TRUE", []any{}
	if s := strings.TrimSpace(q); s != "" {
		where, args = "(h.name ILIKE '%'||$1||'%' OR h.place ILIKE '%'||$1||'%')", []any{s}
	}

	var total int64
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM hotels h WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count hotels: %w", err)
	}

	query := adminHotelSelect + ` WHERE ` + where + ` ORDER BY h.created_at DESC, h.id DESC
		LIMIT $` + fmt.Sprint(len(args)+1) + ` OFFSET $` + fmt.Sprint(len(args)+2)
	qargs := append(append([]any{}, args...), page.Limit, page.Offset)

	rows, err := r.db.Query(ctx, query, qargs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list hotels: %w", err)
	}
	defer rows.Close()

	var out []AdminHotel
	for rows.Next() {
		h, err := scanAdminHotel(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan hotel: %w", err)
		}
		out = append(out, *h)
	}
	return out, total, rows.Err()
}

// GetHotel returns one hotel with its rooms (admin room shape).
func (r *Repository) GetHotel(ctx context.Context, id int64) (*AdminHotel, error) {
	h, err := scanAdminHotel(r.db.QueryRow(ctx, adminHotelSelect+` WHERE h.id = $1`, id))
	if err != nil {
		return nil, err
	}
	rooms, err := r.ListRooms(ctx, id)
	if err != nil {
		return nil, err
	}
	h.Rooms = rooms
	return h, nil
}

// CreateHotel inserts a hotel and returns it. gallery_urls/facilities
// are TEXT[] columns — pgx v5 encodes Go []string directly (a nil slice
// becomes an empty array, matching the column default).
func (r *Repository) CreateHotel(ctx context.Context, req CreateHotelRequest) (*AdminHotel, error) {
	var h AdminHotel
	err := r.db.QueryRow(ctx, `
		INSERT INTO hotels (name, place, country, region, image_url, gallery_urls, rating, facilities, place_id, source)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,COALESCE($6::text[],'{}'),$7,COALESCE($8::text[],'{}'),$9,'admin')
		RETURNING id, name, place, country, COALESCE(region,''), image_url,
		          COALESCE(gallery_urls,'{}'), rating, COALESCE(facilities,'{}'),
		          place_id, source, created_at`,
		req.Name, req.Place, req.Country, req.Region, req.ImageURL,
		req.GalleryURLs, req.Rating, req.Facilities, req.PlaceID,
	).Scan(&h.ID, &h.Name, &h.Place, &h.Country, &h.Region, &h.ImageURL,
		&h.GalleryURLs, &h.Rating, &h.Facilities, &h.PlaceID, &h.Source, &h.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert hotel: %w", err)
	}
	return &h, nil
}

// UpdateHotel patches supplied fields only.
func (r *Repository) UpdateHotel(ctx context.Context, id int64, req UpdateHotelRequest) error {
	var sets []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if req.Name != nil {
		sets = append(sets, "name = "+arg(*req.Name))
	}
	if req.Place != nil {
		sets = append(sets, "place = "+arg(*req.Place))
	}
	if req.Country != nil {
		sets = append(sets, "country = "+arg(*req.Country))
	}
	if req.Region != nil {
		sets = append(sets, "region = NULLIF("+arg(*req.Region)+",'')")
	}
	if req.ImageURL != nil {
		sets = append(sets, "image_url = "+arg(*req.ImageURL))
	}
	if req.GalleryURLs != nil {
		sets = append(sets, "gallery_urls = COALESCE("+arg(*req.GalleryURLs)+"::text[], '{}')")
	}
	if req.Rating != nil {
		sets = append(sets, "rating = "+arg(*req.Rating))
	}
	if req.Facilities != nil {
		sets = append(sets, "facilities = COALESCE("+arg(*req.Facilities)+"::text[], '{}')")
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	tag, err := r.db.Exec(ctx,
		`UPDATE hotels SET `+strings.Join(sets, ", ")+` WHERE id = $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// CountHotelBookings counts every booking (any status) of a hotel — the
// delete guard: bookings.hotel_id is ON DELETE CASCADE, so deleting a
// hotel would silently destroy booking history.
func (r *Repository) CountHotelBookings(ctx context.Context, hotelID int64) (int64, error) {
	var n int64
	err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM bookings WHERE hotel_id = $1`, hotelID).Scan(&n)
	return n, err
}

// DeleteHotel removes a hotel (rooms cascade with it).
func (r *Repository) DeleteHotel(ctx context.Context, id int64) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM hotels WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

const adminRoomSelect = `
	SELECT r.id, r.hotel_id, r.name, r.price_mmk, r.capacity, r.image_url, COALESCE(r.description,''),
	       r.total_units, r.is_active,
	       (SELECT count(*) FROM bookings b WHERE b.room_id = r.id AND b.status IN ('pending','confirmed'))
	FROM hotel_rooms r`

func scanAdminRoom(row pgx.Row) (*AdminRoom, error) {
	var rm AdminRoom
	err := row.Scan(&rm.ID, &rm.HotelID, &rm.Name, &rm.PriceMMK, &rm.Capacity, &rm.ImageURL,
		&rm.Description, &rm.TotalUnits, &rm.IsActive, &rm.ActiveBookings)
	if err != nil {
		return nil, err
	}
	return &rm, nil
}

// ListRooms returns the admin room rows of a hotel.
func (r *Repository) ListRooms(ctx context.Context, hotelID int64) ([]AdminRoom, error) {
	rows, err := r.db.Query(ctx, adminRoomSelect+` WHERE r.hotel_id = $1 ORDER BY r.id`, hotelID)
	if err != nil {
		return nil, fmt.Errorf("list rooms: %w", err)
	}
	defer rows.Close()
	var out []AdminRoom
	for rows.Next() {
		rm, err := scanAdminRoom(rows)
		if err != nil {
			return nil, fmt.Errorf("scan room: %w", err)
		}
		out = append(out, *rm)
	}
	return out, rows.Err()
}

// GetRoom returns one room of a hotel (scoped — a room of another hotel
// is "not found").
func (r *Repository) GetRoom(ctx context.Context, hotelID, roomID int64) (*AdminRoom, error) {
	return scanAdminRoom(r.db.QueryRow(ctx,
		adminRoomSelect+` WHERE r.hotel_id = $1 AND r.id = $2`, hotelID, roomID))
}

// CreateRoom inserts a room type under a hotel.
func (r *Repository) CreateRoom(ctx context.Context, hotelID int64, req RoomRequest) (*AdminRoom, error) {
	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	units := req.TotalUnits
	if units <= 0 {
		units = 1
	}
	capacity := req.Capacity
	if capacity <= 0 {
		capacity = 2
	}
	var rm AdminRoom
	err := r.db.QueryRow(ctx, `
		INSERT INTO hotel_rooms (hotel_id, name, price_mmk, capacity, image_url, description, total_units, is_active)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8)
		RETURNING id, hotel_id, name, price_mmk, capacity, image_url, COALESCE(description,''),
		          total_units, is_active`,
		hotelID, req.Name, req.PriceMMK, capacity, req.ImageURL, req.Description, units, active,
	).Scan(&rm.ID, &rm.HotelID, &rm.Name, &rm.PriceMMK, &rm.Capacity, &rm.ImageURL,
		&rm.Description, &rm.TotalUnits, &rm.IsActive)
	if err != nil {
		return nil, fmt.Errorf("insert room: %w", err)
	}
	return &rm, nil
}

// UpdateRoom patches a room's commercial fields.
func (r *Repository) UpdateRoom(ctx context.Context, hotelID, roomID int64, req RoomRequest) error {
	active := true
	if req.IsActive != nil {
		active = *req.IsActive
	}
	units := req.TotalUnits
	if units <= 0 {
		units = 1
	}
	capacity := req.Capacity
	if capacity <= 0 {
		capacity = 2
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE hotel_rooms
		SET name = $3, price_mmk = $4, capacity = $5, image_url = $6, description = NULLIF($7,''),
		    total_units = $8, is_active = $9
		WHERE hotel_id = $1 AND id = $2`,
		hotelID, roomID, req.Name, req.PriceMMK, capacity, req.ImageURL, req.Description, units, active)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DeleteRoom removes a room. Bookings.room_id is ON DELETE RESTRICT, so
// a room with booking history fails with SQLSTATE 23503 — the service
// maps that to a 409 (deactivate instead).
func (r *Repository) DeleteRoom(ctx context.Context, hotelID, roomID int64) error {
	tag, err := r.db.Exec(ctx,
		`DELETE FROM hotel_rooms WHERE hotel_id = $1 AND id = $2`, hotelID, roomID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// SetRoomInventory updates allotment (total_units) and stop-sell (is_active).
func (r *Repository) SetRoomInventory(ctx context.Context, hotelID, roomID int64, totalUnits int, isActive *bool) error {
	if isActive == nil {
		tag, err := r.db.Exec(ctx, `
			UPDATE hotel_rooms SET total_units = $3
			WHERE hotel_id = $1 AND id = $2`, hotelID, roomID, totalUnits)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return nil
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE hotel_rooms SET total_units = $3, is_active = $4
		WHERE hotel_id = $1 AND id = $2`, hotelID, roomID, totalUnits, *isActive)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ── Users ─────────────────────────────────────────────────────────────

// ListUsers returns users (optionally filtered by role or q) + total.
func (r *Repository) ListUsers(ctx context.Context, q, role string, page pagination.Page) ([]AdminUser, int64, error) {
	var conds []string
	var args []any
	if s := strings.TrimSpace(q); s != "" {
		args = append(args, s)
		conds = append(conds, fmt.Sprintf("(name ILIKE '%%'||$%d||'%%' OR email ILIKE '%%'||$%d||'%%')", len(args), len(args)))
	}
	if role != "" {
		args = append(args, role)
		conds = append(conds, fmt.Sprintf("role = $%d", len(args)))
	}
	where := "TRUE"
	if len(conds) > 0 {
		where = strings.Join(conds, " AND ")
	}

	var total int64
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM users WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count users: %w", err)
	}

	query := `
		SELECT u.id, COALESCE(u.firebase_uid,''), u.name, u.email, u.role, COALESCE(u.phone,''),
		       (SELECT count(*) FROM bookings b WHERE b.user_id = u.id), u.created_at
		FROM users u WHERE ` + where + ` ORDER BY u.created_at DESC, u.id DESC
		LIMIT $` + fmt.Sprint(len(args)+1) + ` OFFSET $` + fmt.Sprint(len(args)+2)
	qargs := append(append([]any{}, args...), page.Limit, page.Offset)

	rows, err := r.db.Query(ctx, query, qargs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []AdminUser
	for rows.Next() {
		var u AdminUser
		if err := rows.Scan(&u.ID, &u.FirebaseUID, &u.Name, &u.Email, &u.Role, &u.Phone,
			&u.TotalBookings, &u.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// GetUser returns one user row or pgx.ErrNoRows.
func (r *Repository) GetUser(ctx context.Context, id int64) (*AdminUser, error) {
	var u AdminUser
	err := r.db.QueryRow(ctx, `
		SELECT u.id, COALESCE(u.firebase_uid,''), u.name, u.email, u.role, COALESCE(u.phone,''),
		       (SELECT count(*) FROM bookings b WHERE b.user_id = u.id), u.created_at
		FROM users u WHERE u.id = $1`, id,
	).Scan(&u.ID, &u.FirebaseUID, &u.Name, &u.Email, &u.Role, &u.Phone, &u.TotalBookings, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// SetUserRole writes users.role (the revocable RBAC source of truth).
func (r *Repository) SetUserRole(ctx context.Context, id int64, role string) error {
	tag, err := r.db.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, id, role)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ── Stats ─────────────────────────────────────────────────────────────

// StatsOverview aggregates the dashboard counters.
func (r *Repository) StatsOverview(ctx context.Context) (*OverviewStats, error) {
	var s OverviewStats
	err := r.db.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'pending'),
		       count(*) FILTER (WHERE status = 'confirmed'),
		       count(*) FILTER (WHERE status = 'complete'),
		       count(*) FILTER (WHERE status = 'cancelled'),
		       COALESCE(sum(total_mmk) FILTER (WHERE status = 'complete'), 0),
		       COALESCE(sum(total_mmk) FILTER (WHERE status IN ('pending','confirmed')), 0)
		FROM bookings`).Scan(&s.PendingBookings, &s.ConfirmedBookings, &s.CompleteBookings,
		&s.CancelledBookings, &s.RevenueMMK, &s.PendingValueMMK)
	if err != nil {
		return nil, fmt.Errorf("booking stats: %w", err)
	}
	s.TotalBookings = s.PendingBookings + s.ConfirmedBookings + s.CompleteBookings + s.CancelledBookings
	s.ActiveHolds = s.PendingBookings + s.ConfirmedBookings

	err = r.db.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM hotels),
		       (SELECT count(*) FROM hotel_rooms),
		       (SELECT COALESCE(sum(total_units), 0) FROM hotel_rooms WHERE is_active),
		       (SELECT count(*) FROM users)`).
		Scan(&s.Hotels, &s.Rooms, &s.UnitsForSale, &s.Users)
	if err != nil {
		return nil, fmt.Errorf("catalog stats: %w", err)
	}
	return &s, nil
}

// Occupancy returns per-night booked counts for [from, to) — the same
// checkout-exclusive semantics as the exclusion constraint (a night is
// occupied when check_in <= night < check_out).
func (r *Repository) Occupancy(ctx context.Context, from, to string, hotelID int64) ([]OccupancyDay, error) {
	query := `
		SELECT to_char(d.day, 'YYYY-MM-DD'),
		       COALESCE(count(b.id), 0),
		       (SELECT COALESCE(sum(total_units), 0) FROM hotel_rooms WHERE is_active
		         AND ($3 = 0 OR hotel_id = $3))
		FROM generate_series($1::date, $2::date - 1, '1 day') d(day)
		LEFT JOIN bookings b
		  ON d.day >= b.check_in AND d.day < b.check_out
		 AND b.status IN ('pending','confirmed')
		 AND ($3 = 0 OR b.hotel_id = $3)
		GROUP BY d.day ORDER BY d.day`
	rows, err := r.db.Query(ctx, query, from, to, hotelID)
	if err != nil {
		return nil, fmt.Errorf("occupancy: %w", err)
	}
	defer rows.Close()

	var out []OccupancyDay
	for rows.Next() {
		var day OccupancyDay
		if err := rows.Scan(&day.Date, &day.BookedNights, &day.UnitsForSale); err != nil {
			return nil, fmt.Errorf("scan occupancy: %w", err)
		}
		if day.UnitsForSale > 0 {
			day.OccupancyPct = float64(day.BookedNights) / float64(day.UnitsForSale) * 100
		}
		out = append(out, day)
	}
	return out, rows.Err()
}

// ── Audit ─────────────────────────────────────────────────────────────

// InsertAudit records an admin mutation. Details is JSON-serialized.
func (r *Repository) InsertAudit(ctx context.Context, adminID int64, action, entityType string, entityID int64, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	b, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("marshal audit details: %w", err)
	}
	_, err = r.db.Exec(ctx, `
		INSERT INTO admin_audit_logs (admin_id, action, entity_type, entity_id, details)
		VALUES ($1, $2, $3, $4, $5::jsonb)`,
		adminID, action, entityType, entityID, string(b))
	return err
}

// ListAuditLogs returns audit entries (newest first) + total count.
func (r *Repository) ListAuditLogs(ctx context.Context, f AuditFilter) ([]AuditLog, int64, error) {
	var conds []string
	var args []any
	if f.EntityType != "" {
		args = append(args, f.EntityType)
		conds = append(conds, fmt.Sprintf("a.entity_type = $%d", len(args)))
	}
	if f.EntityID > 0 {
		args = append(args, f.EntityID)
		conds = append(conds, fmt.Sprintf("a.entity_id = $%d", len(args)))
	}
	where := "TRUE"
	if len(conds) > 0 {
		where = strings.Join(conds, " AND ")
	}

	var total int64
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM admin_audit_logs a WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count audit logs: %w", err)
	}

	query := `
		SELECT a.id, a.admin_id, COALESCE(u.email,''), a.action, a.entity_type, a.entity_id,
		       COALESCE(a.details, '{}'::jsonb), a.created_at
		FROM admin_audit_logs a LEFT JOIN users u ON u.id = a.admin_id
		WHERE ` + where + ` ORDER BY a.created_at DESC, a.id DESC
		LIMIT $` + fmt.Sprint(len(args)+1) + ` OFFSET $` + fmt.Sprint(len(args)+2)
	qargs := append(append([]any{}, args...), f.Page.Limit, f.Page.Offset)

	rows, err := r.db.Query(ctx, query, qargs...)
	if err != nil {
		return nil, 0, fmt.Errorf("list audit logs: %w", err)
	}
	defer rows.Close()

	var out []AuditLog
	for rows.Next() {
		var a AuditLog
		var details []byte
		if err := rows.Scan(&a.ID, &a.AdminID, &a.AdminEmail, &a.Action, &a.EntityType,
			&a.EntityID, &details, &a.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("scan audit log: %w", err)
		}
		if len(details) > 0 {
			_ = json.Unmarshal(details, &a.Details)
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// isFKViolation reports SQLSTATE 23503 (foreign_key_violation) or 23001
// (restrict_violation) — Postgres uses 23001 for ON DELETE RESTRICT,
// which is what bookings.room_id uses.
func isFKViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23001")
}

// IsExclusionViolation reports SQLSTATE 23P01 — the no_double_booking
// constraint rejecting an admin edit that would overlap another hold.
func IsExclusionViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}
