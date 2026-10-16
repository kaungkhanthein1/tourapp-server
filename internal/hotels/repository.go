package hotels

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the data-access boundary for hotels.
// Swap in an external inventory API (Makcorps etc.) without touching service/handler.
type Repository interface {
	ListHotels(ctx context.Context, f Filter) ([]Hotel, error)
	CountHotels(ctx context.Context, f Filter) (int64, error)
	GetHotel(ctx context.Context, id int64) (*Hotel, error)
	ListRooms(ctx context.Context, hotelID int64) ([]Room, error)
	// InsertHotel persists an externally-sourced hotel (hybrid fallback cache).
	InsertHotel(ctx context.Context, h *Hotel) (int64, error)
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
		conds []string
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
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// InsertHotel caches an externally-sourced hotel into Neon (hybrid fallback).
func (r *PostgresRepository) InsertHotel(ctx context.Context, h *Hotel) (int64, error) {
	if h.GalleryURLs == nil {
		h.GalleryURLs = []string{}
	}
	if h.Facilities == nil {
		h.Facilities = []string{}
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO hotels (name, place, country, region, image_url, gallery_urls, rating, facilities, place_id)
		VALUES ($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8,$9)
		ON CONFLICT DO NOTHING
		RETURNING id`,
		h.Name, h.Place, h.Country, h.Region, h.ImageURL, h.GalleryURLs, h.Rating, h.Facilities, h.PlaceID,
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
