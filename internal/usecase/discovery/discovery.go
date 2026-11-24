// Package discovery answers what customers ask first: which vendors are near me, and
// what do they sell.
package discovery

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/persistent"
)

// Limits of a search.
const (
	DefaultRadiusM = 2000
	MaxRadiusM     = 50000
	DefaultLimit   = 20
	MaxLimit       = 50
	maxTextLength  = 80
	// openNowOverfetch fetches extra candidates when filtering by open now, because the
	// hours are judged after the distance search.
	openNowOverfetch = 3
)

// Store is what the use case needs from storage.
type Store interface {
	NearbyOutlets(ctx context.Context, q persistent.NearbyQuery) ([]persistent.NearbyRow, error)
	PublicOutlet(ctx context.Context, id string) (*entity.Outlet, error)
	PublicMenu(ctx context.Context, outletID string) (*entity.Menu, error)
	Ratings(ctx context.Context, outletIDs []string) (map[string]entity.Rating, error)
}

// UseCase is the discovery service.
type UseCase struct {
	store Store
	now   func() time.Time
}

// New builds the use case.
func New(store Store) *UseCase { return &UseCase{store: store, now: time.Now} }

// Query is a nearby search.
type Query struct {
	Latitude  float64
	Longitude float64
	RadiusM   float64
	OpenNow   bool
	Text      string
	Limit     int
}

// Location is where an outlet is shown.
type Location struct {
	Mode       string     `json:"mode"` // FIXED, LIVE or SPOT
	Latitude   float64    `json:"latitude"`
	Longitude  float64    `json:"longitude"`
	AccuracyM  *float64   `json:"accuracyM,omitempty"`
	HeadingDeg *float64   `json:"headingDeg,omitempty"`
	Moving     bool       `json:"moving,omitempty"`
	RecordedAt *time.Time `json:"recordedAt,omitempty"`
	Stale      bool       `json:"stale"` // show "last seen" instead of a live dot
}

// Outlet is what the app shows for one outlet.
type Outlet struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Merchant        string    `json:"merchant"`
	Address         *string   `json:"address"`
	Phone           *string   `json:"phone"`
	LocationMode    string    `json:"locationMode"`
	Location        *Location `json:"location"`
	DistanceM       *int      `json:"distanceMeters,omitempty"`
	IsOpenNow       bool      `json:"isOpenNow"`
	AcceptingOrders bool      `json:"acceptingOrders"`
	// OnlinePayment is whether customers can pay in the app (otherwise they pay at pickup).
	OnlinePayment   bool           `json:"acceptsOnlinePayment"`
	PrepTimeMinutes int            `json:"prepTimeMinutes"`
	Timezone        string         `json:"timezone"`
	Hours           []entity.Hours `json:"hours"`
	Today           *Today         `json:"today"`
	Rating          *entity.Rating `json:"rating"` // nil until the first review
}

// Today is a vendor's current working day.
type Today struct {
	StartedAt *time.Time `json:"startedAt"`
	SpotName  *string    `json:"spotName"`
}

// MenuView is an outlet's menu as the POS published it.
type MenuView struct {
	CatalogVersion int             `json:"catalogVersion"`
	FetchedAt      time.Time       `json:"fetchedAt"`
	Menu           json.RawMessage `json:"menu"`
}

// Nearby lists outlets around a point, nearest first.
func (u *UseCase) Nearby(ctx context.Context, q Query) ([]Outlet, error) {
	if q.RadiusM <= 0 {
		q.RadiusM = DefaultRadiusM
	}

	q.RadiusM = min(q.RadiusM, MaxRadiusM)

	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}

	q.Limit = min(q.Limit, MaxLimit)
	text := strings.TrimSpace(q.Text)

	if len([]rune(text)) > maxTextLength {
		text = string([]rune(text)[:maxTextLength])
	}

	fetch := q.Limit
	if q.OpenNow {
		fetch = min(q.Limit*openNowOverfetch, MaxLimit*openNowOverfetch)
	}

	rows, err := u.store.NearbyOutlets(ctx, persistent.NearbyQuery{
		Latitude: q.Latitude, Longitude: q.Longitude, RadiusM: q.RadiusM, Text: text, Limit: fetch,
	})
	if err != nil {
		return nil, err
	}

	now := u.now()
	out := make([]Outlet, 0, len(rows))

	for _, r := range rows {
		if q.OpenNow && !r.Outlet.IsOpenNow(now) {
			continue
		}

		view := u.view(r.Outlet, now)
		if view.Location == nil || r.Outlet.Pos.Freshness(now) == "hidden" {
			continue
		}

		d := int(r.DistanceM + 0.5)
		view.DistanceM = &d

		out = append(out, view)
		if len(out) == q.Limit {
			break
		}
	}

	ids := make([]string, 0, len(out))
	for _, o := range out {
		ids = append(ids, o.ID)
	}

	ratings, err := u.store.Ratings(ctx, ids)
	if err != nil {
		return nil, err
	}

	for i := range out {
		if r, ok := ratings[out[i].ID]; ok {
			out[i].Rating = &r
		}
	}

	return out, nil
}

// Outlet returns one listed outlet.
func (u *UseCase) Outlet(ctx context.Context, id string) (Outlet, error) {
	o, err := u.store.PublicOutlet(ctx, id)
	if err != nil {
		return Outlet{}, err
	}

	v := u.view(o, u.now())

	ratings, err := u.store.Ratings(ctx, []string{o.ID})
	if err != nil {
		return Outlet{}, err
	}

	if r, ok := ratings[o.ID]; ok {
		v.Rating = &r
	}

	return v, nil
}

// Menu returns the stored menu of a listed outlet.
func (u *UseCase) Menu(ctx context.Context, outletID string) (MenuView, error) {
	m, err := u.store.PublicMenu(ctx, outletID)
	if err != nil {
		return MenuView{}, err
	}

	return MenuView{CatalogVersion: m.CatalogVersion, FetchedAt: m.FetchedAt, Menu: m.Payload}, nil
}

func (u *UseCase) view(o *entity.Outlet, now time.Time) Outlet {
	v := Outlet{
		ID: o.ID, Name: o.Name, Merchant: o.MerchantName, Address: o.Address, Phone: o.Phone,
		LocationMode: o.LocationMode, IsOpenNow: o.IsOpenNow(now), AcceptingOrders: o.AcceptingOrders(now), OnlinePayment: o.OnlinePayment,
		PrepTimeMinutes: o.PrepTimeMinutes, Timezone: o.Timezone, Hours: o.Hours,
	}
	if v.Hours == nil {
		v.Hours = []entity.Hours{}
	}

	if p := o.Pos; p != nil {
		v.Location = &Location{
			Mode: p.Mode, Latitude: p.Latitude, Longitude: p.Longitude, AccuracyM: p.AccuracyM,
			HeadingDeg: p.HeadingDeg, Moving: p.Moving, RecordedAt: p.RecordedAt,
			Stale: p.Freshness(now) == "stale",
		}
	}

	if o.Session.Active {
		today := &Today{StartedAt: o.Session.StartedAt}
		if sp := o.Session.Spot; sp != nil && sp.Name != "" {
			today.SpotName = &sp.Name
		}

		v.Today = today
	}

	return v
}
