package hotels

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the data-access boundary for hotels.
// Swap in an external inventory API without touching service/handler.
type Repository interface {
	ListHotels(ctx context.Context, f Filter) ([]Hotel, error)
	CountHotels(ctx context.Context, f Filter) (int64, error)
	GetHotel(ctx context.Context, id int64) (*Hotel, error)
	ListRooms(ctx context.Context, hotelID int64) ([]Room, error)
	// InsertHotel caches an externally-sourced hotel (hybrid fallback).
	// It returns the cached row id and whether a new row was created, so
	// the caller only seeds hotel_rooms for newly created hotels.
	InsertHotel(ctx context.Context, h *Hotel) (id int64, created bool, err error)
	// InsertRoom persists a room for an externally-sourced hotel.
	InsertRoom(ctx context.Context, rm *Room) (int64, error)
}

// PostgresRepository reads Neon PostgreSQL.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

var _ Repository = (*PostgresRepository)(nil)

func (r *PostgresRepository) ListHotels(ctx context.Context, f Filter) ([]Hotel, error) {
	where, args := buildHotelWhere(f)

	query := `SELECT id, name, place, country, COALESCE(region,''), image_url, gallery_urls,
	                 rating, facilities, place_id, created_at
	          FROM hotels` + where + " ORDER BY rating DESC, id ASC"

	args = append(args, f.Page.Limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	args = append(args, f.Page.Offset)
	query += fmt.Sprintf(" OFFSET $%d", len(args))

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHotels(rows)
}

func (r *PostgresRepository) CountHotels(ctx context.Context, f Filter) (int64, error) {
	where, args := buildHotelWhere(f)
	var n int64
	err := r.pool.QueryRow(ctx, "SELECT count(*) FROM hotels"+where, args...).Scan(&n)
	return n, err
}

func buildHotelWhere(f Filter) (string, []any) {
	var (
		conds = []string{"source <> 'seed'"}
		args  []any
	)
	if f.Place != "" {
		args = append(args, f.Place)
		conds = append(conds, fmt.Sprintf("place = $%d", len(args)))
	}
	if f.Query != "" {
		args = append(args, "%"+strings.ToLower(f.Query)+"%")
		conds = append(conds, fmt.Sprintf("(LOWER(name) LIKE $%d OR LOWER(place) LIKE $%d)", len(args), len(args)))
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// InsertHotel caches an externally-sourced hotel into Neon (hybrid fallback).
// It is an idempotent get-or-create keyed on (name, place) over live rows
// only: curated seed data is never clobbered, and a seed twin with the
// same name gets its own live row instead of masking the cache write.
func (r *PostgresRepository) InsertHotel(ctx context.Context, h *Hotel) (int64, bool, error) {
	if h.GalleryURLs == nil {
		h.GalleryURLs = []string{}
	}
	if h.Facilities == nil {
		h.Facilities = []string{}
	}

	var id int64
	err := r.pool.QueryRow(ctx,
		`SELECT id FROM hotels WHERE LOWER(name) = LOWER($1) AND LOWER(place) = LOWER($2) AND source <> 'seed'`,
		h.Name, h.Place,
	).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, false, err
	}

	err = r.pool.QueryRow(ctx, `
		INSERT INTO hotels (name, place, country, region, image_url, gallery_urls, rating, facilities, place_id, source)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9,'openstreetmap')
		ON CONFLICT DO NOTHING
		RETURNING id`,
		h.Name, h.Place, h.Country, h.Region, h.ImageURL, h.GalleryURLs, h.Rating, h.Facilities, h.PlaceID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Lost a concurrent cache race — reuse the winner's row.
		if serr := r.pool.QueryRow(ctx,
			`SELECT id FROM hotels WHERE LOWER(name) = LOWER($1) AND LOWER(place) = LOWER($2) AND source <> 'seed'`,
			h.Name, h.Place,
		).Scan(&id); serr != nil {
			return 0, false, serr
		}
		return id, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// InsertRoom caches a room for an externally-sourced hotel into Neon.
func (r *PostgresRepository) InsertRoom(ctx context.Context, rm *Room) (int64, error) {
	if rm.Capacity <= 0 {
		rm.Capacity = 2
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO hotel_rooms (hotel_id, name, price_mmk, capacity, image_url, description)
		VALUES ($1,$2,$3,$4,$5,NULLIF($6,''))
		RETURNING id`,
		rm.HotelID, rm.Name, rm.PriceMMK, rm.Capacity, rm.ImageURL, rm.Description,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (r *PostgresRepository) GetHotel(ctx context.Context, id int64) (*Hotel, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, name, place, country, COALESCE(region,''), image_url, gallery_urls,
		       rating, facilities, place_id, created_at
		FROM hotels WHERE id = $1`, id)

	h, err := scanHotel(row)
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

func (r *PostgresRepository) ListRooms(ctx context.Context, hotelID int64) ([]Room, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, hotel_id, name, price_mmk, capacity, image_url, COALESCE(description,'')
		FROM hotel_rooms WHERE hotel_id = $1
		ORDER BY price_mmk ASC`, hotelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Room
	for rows.Next() {
		var rm Room
		if err := rows.Scan(&rm.ID, &rm.HotelID, &rm.Name, &rm.PriceMMK, &rm.Capacity, &rm.ImageURL, &rm.Description); err != nil {
			return nil, err
		}
		out = append(out, rm)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanHotel(row rowScanner) (*Hotel, error) {
	var h Hotel
	if err := row.Scan(&h.ID, &h.Name, &h.Place, &h.Country, &h.Region, &h.ImageURL,
		&h.GalleryURLs, &h.Rating, &h.Facilities, &h.PlaceID, &h.CreatedAt); err != nil {
		return nil, err
	}
	return &h, nil
}

func scanHotels(rows pgx.Rows) ([]Hotel, error) {
	var out []Hotel
	for rows.Next() {
		h, err := scanHotel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}
