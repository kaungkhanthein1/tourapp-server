package places

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the data-access boundary for the places domain.
// PostgresRepository is the initial implementation; an ExternalAPIRepository
// (Google Places / Makcorps) can be swapped in without touching service/handler.
type Repository interface {
	ListPlaces(ctx context.Context, f PlaceFilter) ([]Place, error)
	CountPlaces(ctx context.Context, f PlaceFilter) (int64, error)
	GetPlace(ctx context.Context, id int64) (*Place, error)
	ListTrending(ctx context.Context, limit int) ([]Place, error)
	ListCategories(ctx context.Context) ([]Category, error)
	ListFoods(ctx context.Context, popularOnly bool) ([]Food, error)
	ListFavorites(ctx context.Context, userID int64) ([]Favorite, error)
	AddFavorite(ctx context.Context, userID, placeID int64, note string) error
	RemoveFavorite(ctx context.Context, userID, placeID int64) error
	// InsertPlace persists an externally-sourced place (hybrid fallback cache).
	InsertPlace(ctx context.Context, p *Place) (int64, error)
}

// PostgresRepository reads/writes Neon PostgreSQL.
type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

var _ Repository = (*PostgresRepository)(nil)

func (r *PostgresRepository) ListPlaces(ctx context.Context, f PlaceFilter) ([]Place, error) {
	where, args := buildPlaceWhere(f)

	query := "SELECT id, name, country, COALESCE(region,''), category, image_url, gallery_urls, rating, is_popular, is_trending, COALESCE(description,''), created_at FROM places" + where +
		" ORDER BY rating DESC, id ASC"

	args = append(args, f.Page.Limit)
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	args = append(args, f.Page.Offset)
	query += fmt.Sprintf(" OFFSET $%d", len(args))

	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPlaces(rows)
}

func (r *PostgresRepository) CountPlaces(ctx context.Context, f PlaceFilter) (int64, error) {
	where, args := buildPlaceWhere(f)
	var n int64
	err := r.pool.QueryRow(ctx, "SELECT count(*) FROM places"+where, args...).Scan(&n)
	return n, err
}

func buildPlaceWhere(f PlaceFilter) (string, []any) {
	var (
		conds []string
		args  []any
	)
	if f.Category != "" {
		args = append(args, f.Category)
		conds = append(conds, fmt.Sprintf("category = $%d", len(args)))
	}
	if f.Query != "" {
		args = append(args, "%"+strings.ToLower(f.Query)+"%")
		conds = append(conds, fmt.Sprintf("(LOWER(name) LIKE $%d OR LOWER(country) LIKE $%d OR LOWER(COALESCE(region,'')) LIKE $%d)", len(args), len(args), len(args)))
	}
	if f.IsPopular != nil {
		args = append(args, *f.IsPopular)
		conds = append(conds, fmt.Sprintf("is_popular = $%d", len(args)))
	}
	if f.IsTrending != nil {
		args = append(args, *f.IsTrending)
		conds = append(conds, fmt.Sprintf("is_trending = $%d", len(args)))
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (r *PostgresRepository) GetPlace(ctx context.Context, id int64) (*Place, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, name, country, COALESCE(region,''), category, image_url, gallery_urls,
		       rating, is_popular, is_trending, COALESCE(description,''), created_at
		FROM places WHERE id = $1`, id)

	var p Place
	if err := row.Scan(&p.ID, &p.Name, &p.Country, &p.Region, &p.Category, &p.ImageURL,
		&p.GalleryURLs, &p.Rating, &p.IsPopular, &p.IsTrending, &p.Description, &p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PostgresRepository) ListTrending(ctx context.Context, limit int) ([]Place, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, country, COALESCE(region,''), category, image_url, gallery_urls,
		       rating, is_popular, is_trending, COALESCE(description,''), created_at
		FROM places WHERE is_trending = true
		ORDER BY rating DESC, id ASC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPlaces(rows)
}

func (r *PostgresRepository) ListCategories(ctx context.Context) ([]Category, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, COALESCE(icon,''), sort_order
		FROM categories ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.Icon, &c.SortOrder); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) ListFoods(ctx context.Context, popularOnly bool) ([]Food, error) {
	query := `SELECT id, name, country, image_url, is_popular, COALESCE(description,'') FROM foods`
	if popularOnly {
		query += " WHERE is_popular = true"
	}
	query += " ORDER BY is_popular DESC, id ASC"

	rows, err := r.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Food
	for rows.Next() {
		var f Food
		if err := rows.Scan(&f.ID, &f.Name, &f.Country, &f.ImageURL, &f.IsPopular, &f.Description); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) ListFavorites(ctx context.Context, userID int64) ([]Favorite, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT f.id, f.user_id, f.place_id, COALESCE(f.note,''), f.created_at,
		       p.id, p.name, p.country, COALESCE(p.region,''), p.category, p.image_url,
		       p.gallery_urls, p.rating, p.is_popular, p.is_trending, COALESCE(p.description,''), p.created_at
		FROM favorites f
		JOIN places p ON p.id = f.place_id
		WHERE f.user_id = $1
		ORDER BY f.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Favorite
	for rows.Next() {
		var fav Favorite
		var p Place
		if err := rows.Scan(&fav.ID, &fav.UserID, &fav.PlaceID, &fav.Note, &fav.CreatedAt,
			&p.ID, &p.Name, &p.Country, &p.Region, &p.Category, &p.ImageURL,
			&p.GalleryURLs, &p.Rating, &p.IsPopular, &p.IsTrending, &p.Description, &p.CreatedAt); err != nil {
			return nil, err
		}
		fav.Place = &p
		out = append(out, fav)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) AddFavorite(ctx context.Context, userID, placeID int64, note string) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO favorites (user_id, place_id, note)
		VALUES ($1, $2, NULLIF($3,''))
		ON CONFLICT (user_id, place_id) DO NOTHING`,
		userID, placeID, note)
	return err
}

func (r *PostgresRepository) RemoveFavorite(ctx context.Context, userID, placeID int64) error {
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM favorites WHERE user_id = $1 AND place_id = $2`, userID, placeID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// InsertPlace caches an externally-sourced place into Neon (hybrid fallback).
func (r *PostgresRepository) InsertPlace(ctx context.Context, p *Place) (int64, error) {
	if p.GalleryURLs == nil {
		p.GalleryURLs = []string{}
	}
	var id int64
	err := r.pool.QueryRow(ctx, `
		INSERT INTO places (name, country, region, category, image_url, gallery_urls, rating, is_popular, is_trending, description)
		VALUES ($1,$2,NULLIF($3,''),$4,$5,$6,$7,$8,$9,NULLIF($10,''))
		ON CONFLICT DO NOTHING
		RETURNING id`,
		p.Name, p.Country, p.Region, p.Category, p.ImageURL, p.GalleryURLs,
		p.Rating, p.IsPopular, p.IsTrending, p.Description,
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func scanPlaces(rows pgx.Rows) ([]Place, error) {
	var out []Place
	for rows.Next() {
		var p Place
		if err := rows.Scan(&p.ID, &p.Name, &p.Country, &p.Region, &p.Category, &p.ImageURL,
			&p.GalleryURLs, &p.Rating, &p.IsPopular, &p.IsTrending, &p.Description, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
