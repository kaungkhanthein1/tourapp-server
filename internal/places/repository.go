package places

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the data-access boundary for the places domain.
// PostgresRepository (Neon) is the only implementation — preset-data
// policy: everything is seeded at bootstrap, reads never fetch.
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
	// InsertPlace persists a provider-sourced place (seed-time cache).
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

	query := "SELECT id, name, country, COALESCE(region,''), category, image_url, gallery_urls, rating, is_popular, is_trending, COALESCE(description,''), lat, lon, created_at FROM places" + where +
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
		conds = []string{"source <> 'seed'"}
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
		       rating, is_popular, is_trending, COALESCE(description,''), lat, lon, created_at
		FROM places WHERE id = $1`, id)

	var p Place
	if err := row.Scan(&p.ID, &p.Name, &p.Country, &p.Region, &p.Category, &p.ImageURL,
		&p.GalleryURLs, &p.Rating, &p.IsPopular, &p.IsTrending, &p.Description, &p.Lat, &p.Lon, &p.CreatedAt); err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PostgresRepository) ListTrending(ctx context.Context, limit int) ([]Place, error) {
	if limit <= 0 {
		limit = 10
	}
	// Trending is the live catalog (seeded rows are hidden): newest
	// provider-cached discoveries first.
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, country, COALESCE(region,''), category, image_url, gallery_urls,
		       rating, is_popular, is_trending, COALESCE(description,''), lat, lon, created_at
		FROM places WHERE source <> 'seed'
		ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPlaces(rows)
}

func (r *PostgresRepository) ListCategories(ctx context.Context) ([]Category, error) {
	// Show configured categories only when the live places feed has rows for
	// them. Keep the category table's stable IDs and icons for client keys.
	rows, err := r.pool.Query(ctx, `
		SELECT c.id, c.name, COALESCE(c.icon,''), counts.cnt
		FROM (
			SELECT category, COUNT(*) AS cnt
			FROM places
			WHERE source <> 'seed' AND category <> ''
			GROUP BY category
		) AS counts
		JOIN categories c ON LOWER(c.name) = LOWER(counts.category)
		ORDER BY counts.cnt DESC, c.name
		LIMIT 10`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Category
	for rows.Next() {
		var c Category
		var cnt int64
		if err := rows.Scan(&c.ID, &c.Name, &c.Icon, &cnt); err != nil {
			return nil, err
		}
		c.SortOrder = int(cnt)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) ListFoods(ctx context.Context, popularOnly bool) ([]Food, error) {
	conds := []string{"source <> 'seed'"}
	if popularOnly {
		conds = append(conds, "is_popular = true")
	}
	query := `SELECT id, name, country, image_url, is_popular, COALESCE(description,'') FROM foods WHERE ` +
		strings.Join(conds, " AND ") + " ORDER BY created_at DESC, id DESC"

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
// It is an idempotent get-or-create keyed on (name, country): provider rows
// carry source='openstreetmap' (list queries hide the seeded mock rows), and an
// existing row is promoted when the triggering request asked for
// popular/trending content so those filters hit the database on repeat.
func (r *PostgresRepository) InsertPlace(ctx context.Context, p *Place) (int64, error) {
	if p.GalleryURLs == nil {
		p.GalleryURLs = []string{}
	}

	var id int64
	// Only provider-cached rows are reuse candidates: a hidden seed row may
	// share the name+country (e.g. searching "Rome") — in that case fall
	// through to the INSERT so the app gets a live twin, never mock data.
	err := r.pool.QueryRow(ctx,
		`SELECT id FROM places WHERE LOWER(name) = LOWER($1) AND LOWER(country) = LOWER($2) AND source <> 'seed'`,
		p.Name, p.Country,
	).Scan(&id)
	if err == nil {
		return id, r.promotePlaceFlags(ctx, id, p.IsPopular, p.IsTrending)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}

	err = r.pool.QueryRow(ctx, `
		INSERT INTO places (name, country, region, category, image_url, gallery_urls, rating, is_popular, is_trending, description, lat, lon, source)
		VALUES ($1,$2,NULLIF($3,''),$4,$5,$6,$7,$8,$9,NULLIF($10,''),$11,$12,'openstreetmap')
		ON CONFLICT DO NOTHING
		RETURNING id`,
		p.Name, p.Country, p.Region, p.Category, p.ImageURL, p.GalleryURLs,
		p.Rating, p.IsPopular, p.IsTrending, p.Description, p.Lat, p.Lon,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		// Lost a concurrent cache race — reuse the winner's row.
		if serr := r.pool.QueryRow(ctx,
			`SELECT id FROM places WHERE LOWER(name) = LOWER($1) AND LOWER(country) = LOWER($2) AND source <> 'seed'`,
			p.Name, p.Country,
		).Scan(&id); serr != nil {
			return 0, serr
		}
		return id, r.promotePlaceFlags(ctx, id, p.IsPopular, p.IsTrending)
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// promotePlaceFlags raises is_popular/is_trending on a cached row (never
// lowers them): a popular/trending request bootstraps empty tables, and the
// promotion makes the next identical request resolve from the database
// instead of re-calling the provider.
func (r *PostgresRepository) promotePlaceFlags(ctx context.Context, id int64, popular, trending bool) error {
	if !popular && !trending {
		return nil
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE places SET is_popular = is_popular OR $2, is_trending = is_trending OR $3 WHERE id = $1`,
		id, popular, trending)
	return err
}

func scanPlaces(rows pgx.Rows) ([]Place, error) {
	var out []Place
	for rows.Next() {
		var p Place
		if err := rows.Scan(&p.ID, &p.Name, &p.Country, &p.Region, &p.Category, &p.ImageURL,
			&p.GalleryURLs, &p.Rating, &p.IsPopular, &p.IsTrending, &p.Description, &p.Lat, &p.Lon, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
