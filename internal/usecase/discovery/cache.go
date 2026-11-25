package discovery

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/persistent"
)

const (
	// _minReload is the shortest time between two reloads of the snapshot. A change marks it
	// out of date at once; the next read after this time reloads it. While vendors move,
	// that is the delay between a position event and customers seeing it.
	_minReload = 5 * time.Second

	// _maxAge reloads the snapshot at least this often when it is read, whatever happened:
	// the net under a change that went unseen (another instance without Redis, a manual edit).
	_maxAge = 10 * time.Minute

	// _earthRadiusM is the radius the database's earthdistance module uses, so a distance
	// here equals the one the SQL search gave.
	_earthRadiusM = 6378168.0
)

// Tables whose writes change what customers see here.
var _watched = map[string]struct{}{
	"outlets": {}, "outlet_hours": {}, "outlet_menus": {}, "menu_items": {},
	"merchants": {}, "merchant_gateways": {}, "reviews": {},
}

// Source is what the cache loads from storage.
type Source interface {
	ListPublicOutlets(ctx context.Context) ([]*entity.Outlet, error)
	OutletItemTexts(ctx context.Context) (map[string]string, error)
	AllRatings(ctx context.Context) (map[string]entity.Rating, error)
	PublicMenu(ctx context.Context, outletID string) (*entity.Menu, error)
}

// Cached answers the discovery questions from memory. It is a Store, so the use case does not
// know the difference: the listed outlets, their ratings and the menus a customer opens are
// kept in a snapshot that is reloaded when a write to the tables behind them is committed
// (Invalidate) and at most every few seconds. With the map open on a customer's phone, a
// request then costs no database time, and a serverless database can sleep.
//
// Distances, the live-position cut-off, text search and ordering follow the SQL search they
// replace; opening hours and freshness are still judged at read time by the use case.
type Cached struct {
	src       Source
	now       func() time.Time
	minReload time.Duration

	mu       sync.Mutex
	snap     *snapshot
	loadedAt time.Time
	dirty    bool
}

type snapshot struct {
	outlets []*entity.Outlet
	byID    map[string]*entity.Outlet
	ratings map[string]entity.Rating

	mu    sync.Mutex
	texts map[string]string // loaded on the first text search
	menus map[string]*entity.Menu
}

var _ Store = (*Cached)(nil)

// NewCached builds the cache on a storage source.
func NewCached(src Source) *Cached { return &Cached{src: src, now: time.Now, minReload: _minReload} }

// MinReload sets the shortest time between two reloads (5 seconds by default).
func (c *Cached) MinReload(d time.Duration) *Cached {
	c.minReload = d

	return c
}

// Watches reports whether a write to any of these tables changes what the cache holds.
func Watches(tables []string) bool {
	for _, t := range tables {
		if _, ok := _watched[t]; ok {
			return true
		}
	}

	return false
}

// Invalidate marks the snapshot out of date; it is reloaded by the next read after the minimum reload time.
func (c *Cached) Invalidate() {
	c.mu.Lock()
	c.dirty = true
	c.mu.Unlock()
}

// current returns the snapshot, loading or reloading it when needed. If a reload fails the
// previous snapshot keeps serving.
func (c *Cached) current(ctx context.Context) (*snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	age := now.Sub(c.loadedAt)

	if c.snap != nil && !(c.dirty && age >= c.minReload) && age < _maxAge {
		return c.snap, nil
	}

	outlets, err := c.src.ListPublicOutlets(ctx)
	if err == nil {
		var ratings map[string]entity.Rating

		if ratings, err = c.src.AllRatings(ctx); err == nil {
			s := &snapshot{
				outlets: outlets, byID: make(map[string]*entity.Outlet, len(outlets)),
				ratings: ratings, menus: map[string]*entity.Menu{},
			}

			for _, o := range outlets {
				s.byID[o.ID] = o
			}

			c.snap, c.loadedAt, c.dirty = s, now, false

			return s, nil
		}
	}

	if c.snap != nil {
		return c.snap, nil
	}

	return nil, err
}

// NearbyOutlets finds listed outlets within the radius, nearest first.
func (c *Cached) NearbyOutlets(ctx context.Context, q persistent.NearbyQuery) ([]persistent.NearbyRow, error) {
	s, err := c.current(ctx)
	if err != nil {
		return nil, err
	}

	text := strings.ToLower(strings.TrimSpace(q.Text))

	var items map[string]string

	if text != "" {
		if items, err = s.itemTexts(ctx, c.src); err != nil {
			return nil, err
		}
	}

	now := c.now()
	rows := make([]persistent.NearbyRow, 0, 32)

	for _, o := range s.outlets {
		if o.Pos == nil {
			continue
		}

		d := distanceM(q.Latitude, q.Longitude, o.Pos.Latitude, o.Pos.Longitude)
		if d > q.RadiusM {
			continue
		}

		// A live position nobody refreshed for HideAfter is not shown.
		if o.Pos.Mode == entity.SharingLive && o.Current.RecordedAt != nil && now.Sub(*o.Current.RecordedAt) > entity.HideAfter {
			continue
		}

		if text != "" && !matches(o, text, items[o.ID]) {
			continue
		}

		cp := *o
		rows = append(rows, persistent.NearbyRow{Outlet: &cp, DistanceM: d})
	}

	sort.SliceStable(rows, func(i, j int) bool { return rows[i].DistanceM < rows[j].DistanceM })

	if q.Limit > 0 && len(rows) > q.Limit {
		rows = rows[:q.Limit]
	}

	return rows, nil
}

// PublicOutlet returns an outlet customers may see.
func (c *Cached) PublicOutlet(ctx context.Context, id string) (*entity.Outlet, error) {
	s, err := c.current(ctx)
	if err != nil {
		return nil, err
	}

	o, ok := s.byID[id]
	if !ok {
		return nil, entity.ErrNotFound
	}

	cp := *o

	return &cp, nil
}

// PublicMenu returns the stored menu of an outlet customers may see. A menu is read from the
// database the first time it is opened after a change.
func (c *Cached) PublicMenu(ctx context.Context, outletID string) (*entity.Menu, error) {
	s, err := c.current(ctx)
	if err != nil {
		return nil, err
	}

	if _, ok := s.byID[outletID]; !ok {
		return nil, entity.ErrNotFound
	}

	s.mu.Lock()
	m, ok := s.menus[outletID]
	s.mu.Unlock()

	if ok {
		return m, nil
	}

	if m, err = c.src.PublicMenu(ctx, outletID); err != nil {
		return nil, err
	}

	s.mu.Lock()
	s.menus[outletID] = m
	s.mu.Unlock()

	return m, nil
}

// Ratings returns the rating of each reviewed outlet among the ids.
func (c *Cached) Ratings(ctx context.Context, outletIDs []string) (map[string]entity.Rating, error) {
	s, err := c.current(ctx)
	if err != nil {
		return nil, err
	}

	out := map[string]entity.Rating{}

	for _, id := range outletIDs {
		if r, ok := s.ratings[id]; ok {
			out[id] = r
		}
	}

	return out, nil
}

func (s *snapshot) itemTexts(ctx context.Context, src Source) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.texts != nil {
		return s.texts, nil
	}

	texts, err := src.OutletItemTexts(ctx)
	if err != nil {
		return nil, err
	}

	s.texts = texts

	return texts, nil
}

// matches follows the SQL: the outlet's name, its merchant's name, or one of its menu items.
func matches(o *entity.Outlet, text, items string) bool {
	return strings.Contains(strings.ToLower(o.Name), text) ||
		strings.Contains(strings.ToLower(o.MerchantName), text) ||
		strings.Contains(items, text)
}

// distanceM is the great-circle distance in meters, on the sphere earthdistance uses.
func distanceM(lat1, lng1, lat2, lng2 float64) float64 {
	const rad = math.Pi / 180

	dLat := (lat2 - lat1) * rad
	dLng := (lng2 - lng1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLng/2)*math.Sin(dLng/2)

	return 2 * _earthRadiusM * math.Asin(math.Min(1, math.Sqrt(a)))
}
