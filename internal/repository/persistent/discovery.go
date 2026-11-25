package persistent

import (
	"context"
	"fmt"
	"strings"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// NearbyQuery asks for listed outlets around a point.
type NearbyQuery struct {
	Latitude  float64
	Longitude float64
	RadiusM   float64
	Text      string // optional: matches outlet, merchant or menu item names
	Limit     int
}

// NearbyRow is an outlet with its distance from the search point.
type NearbyRow struct {
	Outlet    *entity.Outlet
	DistanceM float64
}

// escapeLike makes user text safe inside an ILIKE pattern.
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// NearbyOutlets finds listed outlets within the radius, nearest first. A live position
// that has not been refreshed for entity.HideAfter is left out.
func (s *Store) NearbyOutlets(ctx context.Context, q NearbyQuery) ([]NearbyRow, error) {
	args := []any{q.Latitude, q.Longitude, q.RadiusM, q.Limit, entity.HideAfter.Seconds()}

	textFilter := ""

	if t := strings.TrimSpace(q.Text); t != "" {
		args = append(args, "%"+escapeLike(t)+"%")
		textFilter = `AND (o.name ILIKE $6 OR m.name ILIKE $6
			OR EXISTS (SELECT 1 FROM menu_items i WHERE i.outlet_id = o.id AND i.name ILIKE $6))`
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+outletColumns+`,
		  earth_distance(ll_to_earth($1, $2), ll_to_earth(o.pos_lat, o.pos_lng)) AS distance_m`+outletFrom+`
		WHERE o.published AND o.active AND m.status = 'active' AND o.pos_lat IS NOT NULL
		  AND earth_box(ll_to_earth($1, $2), $3) @> ll_to_earth(o.pos_lat, o.pos_lng)
		  AND earth_distance(ll_to_earth($1, $2), ll_to_earth(o.pos_lat, o.pos_lng)) <= $3
		  AND (o.pos_mode IS DISTINCT FROM 'LIVE' OR o.cur_recorded_at IS NULL
		       OR o.cur_recorded_at > now() - make_interval(secs => $5))
		  `+textFilter+`
		ORDER BY distance_m
		LIMIT $4`, args...)
	if err != nil {
		return nil, fmt.Errorf("nearby: %w", err)
	}
	defer rows.Close()

	var out []NearbyRow

	for rows.Next() {
		var distance float64

		o, err := scanOutlet(rows, &distance)
		if err != nil {
			return nil, fmt.Errorf("scan nearby: %w", err)
		}

		out = append(out, NearbyRow{Outlet: o, DistanceM: distance})
	}

	return out, rows.Err()
}

// PublicOutlet returns an outlet customers may see.
func (s *Store) PublicOutlet(ctx context.Context, id string) (*entity.Outlet, error) {
	o, err := scanOutlet(s.pool.QueryRow(ctx, `
		SELECT `+outletColumns+outletFrom+`
		WHERE o.id = $1 AND o.published AND o.active AND m.status = 'active'`, id))
	if err != nil {
		return nil, notFound(err)
	}

	return o, nil
}

// PublicMenu returns the stored menu of an outlet customers may see.
func (s *Store) PublicMenu(ctx context.Context, outletID string) (*entity.Menu, error) {
	var menu entity.Menu

	err := s.pool.QueryRow(ctx, `
		SELECT mn.catalog_version, mn.fetched_at, mn.payload
		FROM outlet_menus mn JOIN outlets o ON o.id = mn.outlet_id JOIN merchants m ON m.id = o.merchant_id
		WHERE mn.outlet_id = $1 AND o.published AND o.active AND m.status = 'active'`, outletID,
	).Scan(&menu.CatalogVersion, &menu.FetchedAt, &menu.Payload)
	if err != nil {
		return nil, notFound(err)
	}

	return &menu, nil
}

// ListPublicOutlets returns every outlet customers may see, for the discovery cache.
func (s *Store) ListPublicOutlets(ctx context.Context) ([]*entity.Outlet, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+outletColumns+outletFrom+`
		WHERE o.published AND o.active AND m.status = 'active'`)
	if err != nil {
		return nil, fmt.Errorf("list public outlets: %w", err)
	}
	defer rows.Close()

	var out []*entity.Outlet

	for rows.Next() {
		o, err := scanOutlet(rows)
		if err != nil {
			return nil, fmt.Errorf("scan public outlet: %w", err)
		}

		out = append(out, o)
	}

	return out, rows.Err()
}

// OutletItemTexts returns, per outlet, its menu item names in lower case joined by a newline:
// what a text search looks through.
func (s *Store) OutletItemTexts(ctx context.Context) (map[string]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT outlet_id::text, string_agg(lower(name), E'\n')
		FROM menu_items GROUP BY outlet_id`)
	if err != nil {
		return nil, fmt.Errorf("item texts: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}

	for rows.Next() {
		var id, text string
		if err := rows.Scan(&id, &text); err != nil {
			return nil, err
		}

		out[id] = text
	}

	return out, rows.Err()
}

// AllRatings returns the rating of every reviewed outlet.
func (s *Store) AllRatings(ctx context.Context) (map[string]entity.Rating, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT outlet_id::text, round(avg(rating)::numeric, 1)::float8, count(*)::int
		FROM reviews GROUP BY outlet_id`)
	if err != nil {
		return nil, fmt.Errorf("all ratings: %w", err)
	}
	defer rows.Close()

	out := map[string]entity.Rating{}

	for rows.Next() {
		var (
			id string
			r  entity.Rating
		)

		if err := rows.Scan(&id, &r.Average, &r.Count); err != nil {
			return nil, err
		}

		out[id] = r
	}

	return out, rows.Err()
}
