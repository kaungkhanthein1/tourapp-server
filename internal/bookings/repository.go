package bookings

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the data-access boundary for bookings.
type Repository interface {
	Create(ctx context.Context, b *Booking) error
	GetByID(ctx context.Context, id int64) (*Booking, error)
	ListByUser(ctx context.Context, userID int64, status string) ([]Booking, error)
	UpdateStatus(ctx context.Context, id int64, status string) error
	Reschedule(ctx context.Context, id int64, checkIn, checkOut string) error
	// RoomPricing returns room name, price, hotel name/place/image for booking creation.
	RoomPricing(ctx context.Context, hotelID, roomID int64) (*RoomPricing, error)
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
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

var _ Repository = (*PostgresRepository)(nil)

func (r *PostgresRepository) Create(ctx context.Context, b *Booking) error {
	return r.pool.QueryRow(ctx, `
		INSERT INTO bookings (user_id, hotel_id, hotel_name, room_name, place, image_url,
		                      price_mmk, check_in, check_out, guests, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::date,$9::date,$10,$11)
		RETURNING id, created_at`,
		b.UserID, b.HotelID, b.HotelName, b.RoomName, b.Place, b.ImageURL,
		b.PriceMMK, b.CheckIn, b.CheckOut, b.Guests, b.Status,
	).Scan(&b.ID, &b.CreatedAt)
}

func (r *PostgresRepository) GetByID(ctx context.Context, id int64) (*Booking, error) {
	b, err := scanBooking(r.pool.QueryRow(ctx, bookingSelect+" WHERE id = $1", id))
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (r *PostgresRepository) ListByUser(ctx context.Context, userID int64, status string) ([]Booking, error) {
	query := bookingSelect + " WHERE user_id = $1"
	args := []any{userID}
	if status != "" {
		args = append(args, status)
		query += fmt.Sprintf(" AND status = $%d", len(args))
	}
	query += " ORDER BY created_at DESC, id DESC"

	rows, err := r.pool.Query(ctx, query, args...)
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
	tag, err := r.pool.Exec(ctx, `UPDATE bookings SET status = $2 WHERE id = $1`, id, status)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *PostgresRepository) Reschedule(ctx context.Context, id int64, checkIn, checkOut string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE bookings SET check_in = $2::date, check_out = $3::date
		WHERE id = $1 AND status = 'pending'`, id, checkIn, checkOut)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func (r *PostgresRepository) RoomPricing(ctx context.Context, hotelID, roomID int64) (*RoomPricing, error) {
	var p RoomPricing
	err := r.pool.QueryRow(ctx, `
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

const bookingSelect = `
	SELECT id, user_id, hotel_id, hotel_name, room_name, place, image_url,
	       price_mmk, check_in::text, check_out::text, guests, status, created_at
	FROM bookings`

type scanner interface {
	Scan(dest ...any) error
}

func scanBooking(row scanner) (*Booking, error) {
	var b Booking
	if err := row.Scan(&b.ID, &b.UserID, &b.HotelID, &b.HotelName, &b.RoomName, &b.Place,
		&b.ImageURL, &b.PriceMMK, &b.CheckIn, &b.CheckOut, &b.Guests, &b.Status, &b.CreatedAt); err != nil {
		return nil, err
	}
	return &b, nil
}
