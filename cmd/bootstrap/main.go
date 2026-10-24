package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"travel-app-backend/internal/database"
	"travel-app-backend/internal/hotels"
	"travel-app-backend/internal/pkg/external"
	"travel-app-backend/internal/places"
)

// One-shot bootstrap: migrates/seeds the schema, optionally loads SerpApi
// place/hotel data or the offline food catalog, then applies place
// curation. Nothing fetches at runtime: cmd/server serves Neon only.
// No server is started — the API stays the user's to run.
func main() {
	_ = godotenv.Load() // same .env the server reads
	dryRun := false
	useSerpapi := false
	seedPlaces := true
	seedHotels := true
	seedFoodCatalog := false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "-dry-run":
			dryRun = true
		case "-serpapi":
			useSerpapi = true
		case "-serpapi-hotels":
			// Hotels-only continuation: the google_maps place queries
			// already ran — re-running them would spend ~18 plan credits
			// for rows the dedupe would mostly skip. The hotel seed itself
			// resumes: only places with no preset rows are queried.
			useSerpapi = true
			seedPlaces = false
		case "-food-catalog":
			// Offline, curated catalog continuation: add dishes only for
			// countries with no live food rows. Existing provider data stays.
			seedPlaces = false
			seedHotels = false
			seedFoodCatalog = true
		}
	}
	if seedFoodCatalog && useSerpapi {
		fail("-food-catalog cannot be combined with SerpApi flags\n")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()

	pool, err := database.NewPool(ctx)
	if err != nil {
		fail("connect: %v", err)
	}
	defer pool.Close()

	if err := database.Migrate(ctx, pool); err != nil {
		fail("migrate: %v", err)
	}
	if err := database.Seed(ctx, pool); err != nil {
		fail("seed: %v", err)
	}

	placesSvc := places.NewService(places.NewPostgresRepository(pool))
	hotelsSvc := hotels.NewService(hotels.NewPostgresRepository(pool))

	start := time.Now()

	// SerpApi seed (opt-in): google_maps attractions + the per-place
	// google_hotels preset inventory. One plan credit per query — the
	// only caller of the paid API. The hotel block is idempotent by
	// label|name dedupe, so repeat runs only pay for uncovered places.
	if useSerpapi {
		seedWithSerpapi(ctx, pool, placesSvc, hotelsSvc, seedPlaces, seedHotels)
	}

	// Catalog-only runs do not mutate the places catalog as a side effect.
	if seedFoodCatalog {
		if err := seedCuratedFoods(ctx, pool, dryRun); err != nil {
			fail("food catalog: %v", err)
		}
	}
	if !seedFoodCatalog {
		curatePlaces(ctx, pool, dryRun)
	}

	stats(ctx, pool)
	fmt.Printf("bootstrap elapsed: %s\n", time.Since(start).Truncate(time.Second))
}

type catalogFood struct {
	Name        string `json:"name"`
	Country     string `json:"country"`
	ImageURL    string `json:"image_url"`
	Description string `json:"description"`
}

// seedCuratedFoods adds the checked-in, photo-backed food catalog only
// where no live food rows exist. It never deletes or replaces provider
// data, so reruns are idempotent and safe after partial completion.
func seedCuratedFoods(ctx context.Context, pool *pgxpool.Pool, dryRun bool) error {
	b, err := os.ReadFile("mock_data/foods_catalog.json")
	if err != nil {
		return fmt.Errorf("read catalog: %w", err)
	}
	var entries []catalogFood
	if err := json.Unmarshal(b, &entries); err != nil {
		return fmt.Errorf("parse catalog: %w", err)
	}
	byCountry := make(map[string][]catalogFood)
	seen := make(map[string]struct{}, len(entries))
	for _, f := range entries {
		f.Name = strings.TrimSpace(f.Name)
		f.Country = strings.TrimSpace(f.Country)
		f.ImageURL = strings.TrimSpace(f.ImageURL)
		f.Description = strings.TrimSpace(f.Description)
		if f.Name == "" || f.Country == "" || !strings.HasPrefix(f.ImageURL, "https://") {
			return fmt.Errorf("invalid catalog row %q (%s): name, country and HTTPS image URL are required", f.Name, f.Country)
		}
		key := strings.ToLower(f.Country + "\x00" + f.Name)
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate catalog dish %q (%s)", f.Name, f.Country)
		}
		seen[key] = struct{}{}
		byCountry[f.Country] = append(byCountry[f.Country], f)
	}
	countries := make([]string, 0, len(byCountry))
	for country := range byCountry {
		countries = append(countries, country)
	}
	sort.Strings(countries)
	for _, country := range countries {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("%s begin transaction: %w", country, err)
		}
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtext('tourapp:food-catalog'), hashtext(LOWER($1)))`, country); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s acquire seed lock: %w", country, err)
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM foods WHERE LOWER(country) = LOWER($1) AND source <> 'seed'
		)`, country).Scan(&exists); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s check existing rows: %w", country, err)
		}
		if exists {
			_ = tx.Rollback(ctx)
			fmt.Printf("food catalog: %s already has live rows — kept\n", country)
			continue
		}
		if dryRun {
			_ = tx.Rollback(ctx)
			fmt.Printf("food catalog: %s would add %d dishes\n", country, len(byCountry[country]))
			continue
		}
		written := 0
		for _, f := range byCountry[country] {
			if _, err := tx.Exec(ctx, `
				INSERT INTO foods (name, country, image_url, is_popular, description, source)
				VALUES ($1, $2, $3, TRUE, NULLIF($4,''), 'catalog')`,
				strings.TrimSpace(f.Name), country, strings.TrimSpace(f.ImageURL), strings.TrimSpace(f.Description)); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("%s insert %q (transaction rolled back): %w", country, f.Name, err)
			}
			written++
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("%s commit transaction: %w", country, err)
		}
		fmt.Printf("food catalog: %s added %d dishes\n", country, written)
	}
	return nil
}

// serpapiPlaceQueries are the google_maps searches behind -serpapi:
// per-anchor attraction sweeps chosen so every Explore chip gains
// curated rows (Parks/Hills/Beaches are the thin chips). One plan credit
// each — 18 queries against the ~250/month plan.
var serpapiPlaceQueries = []string{
	"Paris attractions", "Paris parks", "Paris museums",
	"Rome attractions", "Rome parks", "Rome ruins",
	"Tokyo attractions", "Tokyo parks", "Tokyo shrines",
	"New York attractions", "New York parks", "New York beaches",
	"Bangkok attractions", "Bangkok temples", "Bangkok street food",
	"Denpasar attractions", "Denpasar beaches",
	"Bali volcanoes",
}

// seedWithSerpapi fetches SerpApi places/hotels into Neon
// via the services' forced-seed paths (the plain list calls would serve
// the warm database and never fetch). The two blocks are independent
// flags: -serpapi (places+hotels) and -serpapi-hotels.
// Failures degrade per query — a spent plan or a transient outage
// reports, never aborts, the run.
func seedWithSerpapi(ctx context.Context, pool *pgxpool.Pool, placesSvc *places.Service, hotelsSvc *hotels.Service, withPlaces, withHotels bool) {
	key := strings.TrimSpace(os.Getenv("SERPAPI_KEY"))
	if key == "" {
		fail("SERPAPI_KEY is not set (add it to .env) — -serpapi needs it, one plan credit per query\n")
	}
	client := external.NewSerpAPIClient(key)

	if withPlaces {
		pl := placesSvc.SeedExternal(ctx, client, serpapiPlaceQueries)
		fmt.Printf("serpapi places: %d rows fetched (%d queries)\n", len(pl), len(serpapiPlaceQueries))
	}
	if withHotels {
		seedPresetHotels(ctx, pool, hotelsSvc, client)
	}
}

// placeRef is one place row as the hotel seeder sees it: the exact
// name places are queried under, plus the city/country that build the
// google_hotels text query.
type placeRef struct{ name, region, country string }

// seedPresetHotels seeds one labeled google_hotels query per place row:
// results are stored under the exact places.name, so the place detail's
// place = $1 lookup hits Neon with zero runtime fetching (preset-data
// policy). A place whose first query returns nothing is retried once
// with a plain "<place>, <city> hotels" phrasing; whatever still misses
// is reported — its detail shows an empty hotel list by design.
func seedPresetHotels(ctx context.Context, pool *pgxpool.Pool, hotelsSvc *hotels.Service, searcher external.HotelSearcher) {
	places, err := loadPlaceRefs(ctx, pool)
	if err != nil {
		fmt.Printf("serpapi hotels: load places ERROR %v\n", err)
		return
	}
	byName := map[string]placeRef{}
	for _, p := range places {
		byName[p.name] = p
	}

	// Resume-friendly: only places with zero preset rows are queried, so
	// an interrupted run continues where it stopped instead of paying
	// for labels that already have inventory (one credit per query).
	misses, err := uncoveredPlaces(ctx, pool)
	if err != nil {
		fmt.Printf("serpapi hotels: coverage check ERROR %v\n", err)
		return
	}
	if len(misses) == 0 {
		fmt.Println("serpapi hotels: full coverage — every place has preset hotels")
		return
	}
	first := seedsFor(misses, byName, hotelNearQuery)
	fmt.Printf("serpapi hotels: %d places uncovered, running primary queries\n", len(first))
	fetched := hotelsSvc.SeedLabeled(ctx, searcher, first)
	fmt.Printf("serpapi hotels: %d rows cached from %d queries\n", len(fetched), len(first))

	misses, err = uncoveredPlaces(ctx, pool)
	if err != nil || len(misses) == 0 {
		if err == nil {
			fmt.Println("serpapi hotels: full coverage after primary queries")
		}
		return
	}
	retry := seedsFor(misses, byName, hotelPlainQuery)
	fmt.Printf("serpapi hotels: %d places still uncovered, retrying with variant queries\n", len(retry))
	refetched := hotelsSvc.SeedLabeled(ctx, searcher, retry)
	fmt.Printf("serpapi hotels: %d rows cached from %d retries\n", len(refetched), len(retry))

	still, err := uncoveredPlaces(ctx, pool)
	if err != nil {
		return
	}
	if len(still) == 0 {
		fmt.Println("serpapi hotels: full coverage after retries")
		return
	}
	fmt.Printf("serpapi hotels: %d places have NO preset hotels: %s\n",
		len(still), strings.Join(still, ", "))
}

// seedsFor maps uncovered place names to labeled seeds using the given
// query phrasing; names missing from the place table are skipped.
func seedsFor(names []string, byName map[string]placeRef, phrasing func(placeRef) string) []hotels.LabeledSeed {
	out := make([]hotels.LabeledSeed, 0, len(names))
	for _, name := range names {
		p, ok := byName[name]
		if !ok {
			continue
		}
		out = append(out, hotels.LabeledSeed{
			Label: p.name, Query: phrasing(p), Region: p.region, Country: p.country,
		})
	}
	return out
}

// loadPlaceRefs reads every place row (including source='seed' fixture
// rows) and dedupes by name.
func loadPlaceRefs(ctx context.Context, pool *pgxpool.Pool) ([]placeRef, error) {
	rows, err := pool.Query(ctx, `
		SELECT name, COALESCE(region, ''), COALESCE(country, '')
		FROM places ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []placeRef
	seen := map[string]bool{}
	for rows.Next() {
		var p placeRef
		if err := rows.Scan(&p.name, &p.region, &p.country); err != nil {
			return nil, err
		}
		if seen[p.name] || p.name == "" {
			continue
		}
		seen[p.name] = true
		out = append(out, p)
	}
	return out, rows.Err()
}

// uncoveredPlaces returns place names with zero preset hotel rows —
// the runtime fallback that used to fill them is gone by policy.
func uncoveredPlaces(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT p.name FROM places p
		WHERE NOT EXISTS (SELECT 1 FROM hotels h WHERE h.place = p.name)
		ORDER BY p.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// hotelNearQuery is the primary phrasing — Google understands
// "hotels near <POI>, <city>, <country>" as an area search.
func hotelNearQuery(p placeRef) string {
	return "hotels near " + strings.Join(placeParts(p), ", ")
}

// hotelPlainQuery is the one retry phrasing for uncovered places.
func hotelPlainQuery(p placeRef) string {
	return strings.Join(placeParts(p), ", ") + " hotels"
}

// placeParts returns the non-blank, de-duplicated segments of a query.
func placeParts(p placeRef) []string {
	var out []string
	for _, s := range []string{p.name, p.region, p.country} {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		dup := false
		for _, prev := range out {
			if strings.EqualFold(prev, s) {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []string{p.name}
	}
	return out
}

// curatePlaces applies the feed's final quality bar to every live place
// row: real wiki prose (except Cities) and a subject-appropriate
// Wikimedia file title — all local string checks, no network. Rejected
// rows are deleted — the feed only shows curated cards. With dryRun it
// reports per-chip survival without deleting anything.
func curatePlaces(ctx context.Context, pool *pgxpool.Pool, dryRun bool) {
	rows, err := pool.Query(ctx, `
		SELECT id, name, category, image_url, COALESCE(description, '')
		FROM places WHERE source <> 'seed'`)
	if err != nil {
		fmt.Printf("curate: query ERROR %v\n", err)
		return
	}
	type placeRow struct {
		id          int64
		name        string
		category    string
		image       string
		description string
	}
	var all []placeRow
	for rows.Next() {
		var p placeRow
		if err := rows.Scan(&p.id, &p.name, &p.category, &p.image, &p.description); err != nil {
			rows.Close()
			fmt.Printf("curate: scan ERROR %v\n", err)
			return
		}
		all = append(all, p)
	}
	rows.Close()

	// Per-chip bookkeeping for the report.
	totalByChip := map[string]int{}
	rejectByChip := map[string]int{}
	reasonCount := map[string]int{}
	var rejectIDs []int64

	for _, p := range all {
		totalByChip[p.category]++

		reason := curationReason(p.category, p.image, p.description)
		if reason == "" {
			continue
		}
		rejectByChip[p.category]++
		reasonCount[reason]++
		rejectIDs = append(rejectIDs, p.id)
	}

	fmt.Printf("curate: %d places checked, %d would be rejected\n", len(all), len(rejectIDs))
	for _, cat := range sortedKeys(totalByChip) {
		survive := totalByChip[cat] - rejectByChip[cat]
		fmt.Printf("  %-14s %2d / %2d survive\n", cat, survive, totalByChip[cat])
	}
	for _, r := range sortedKeys(reasonCount) {
		fmt.Printf("  reason %-12s %d\n", r, reasonCount[r])
	}

	if dryRun {
		fmt.Println("curate: DRY RUN — nothing deleted")
		return
	}
	if len(rejectIDs) == 0 {
		return
	}
	tag, err := pool.Exec(ctx, `DELETE FROM places WHERE id = ANY($1)`, rejectIDs)
	if err != nil {
		fmt.Printf("curate: delete ERROR %v\n", err)
		return
	}
	fmt.Printf("curate: deleted %d places\n", tag.RowsAffected())
}

// curationReason returns the first failing check for a place, or "" when
// it passes — pure string checks (CuratedPlaceMedia covers prose length,
// junk file titles and chip mismatches).
func curationReason(category, image, description string) string {
	if !places.CuratedPlaceMedia(category, image, description) {
		if category != "Cities" && len([]rune(description)) < places.MinCuratedProseLen {
			return "thin prose"
		}
		title := external.WikimediaFileTitle(image)
		if title != "" && external.PhotoSubjectJunk(title) {
			return "junk title"
		}
		return "mismatch"
	}
	return ""
}

// sortedKeys returns a map's keys in sorted order for stable reports.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// stats prints the per-table photo coverage — the number this bootstrap
// exists to move: http(s) image URLs (provider-native photos).
func stats(ctx context.Context, pool *pgxpool.Pool) {
	for _, table := range []string{"places", "foods", "hotels"} {
		var total, photos int
		q := fmt.Sprintf(`SELECT count(*) FILTER (WHERE source <> 'seed'),
			count(*) FILTER (WHERE source <> 'seed' AND image_url LIKE 'http%%')
			FROM %s`, table)
		if err := pool.QueryRow(ctx, q).Scan(&total, &photos); err != nil {
			fmt.Printf("stats %s: ERROR %v\n", table, err)
			continue
		}
		fmt.Printf("stats %s: %d cached rows, %d with native photos\n", table, total, photos)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
