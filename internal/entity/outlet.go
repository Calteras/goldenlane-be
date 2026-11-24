// Package entity holds the business types and rules that do not depend on storage or HTTP.
package entity

import (
	"time"
)

// Location modes and sharing choices, as the POS names them.
const (
	ModeFixed  = "FIXED"
	ModeMobile = "MOBILE"

	SharingLive   = "LIVE"
	SharingSpot   = "SPOT"
	SharingHidden = "HIDDEN"

	StatusAuto   = "AUTO"
	StatusOpen   = "OPEN"
	StatusClosed = "CLOSED"
	StatusPaused = "PAUSED"
)

// How fresh a live position must be: past StaleAfter the app should say "last seen",
// past HideAfter the vendor is not shown at all.
const (
	StaleAfter = 3 * time.Minute
	HideAfter  = 10 * time.Minute
)

// Hours is one weekly opening interval in the outlet's timezone.
type Hours struct {
	DayOfWeek int    `json:"dayOfWeek"` // 0 = Sunday
	OpensAt   string `json:"opensAt"`   // HH:MM
	ClosesAt  string `json:"closesAt"`  // HH:MM; earlier than OpensAt ends after midnight
}

// Spot is the place a vendor works from today.
type Spot struct {
	ID        *string  `json:"id"`
	Name      string   `json:"name"`
	Latitude  *float64 `json:"latitude"`
	Longitude *float64 `json:"longitude"`
}

// Session is a mobile vendor's or kiosk's working day.
type Session struct {
	Active    bool
	ID        string
	StartedAt *time.Time
	Sharing   string
	Spot      *Spot
}

// Current is the latest position the POS reported.
type Current struct {
	Latitude   *float64
	Longitude  *float64
	AccuracyM  *float64
	HeadingDeg *float64
	Moving     bool
	RecordedAt *time.Time
}

// Position is where customers find an outlet right now.
type Position struct {
	Latitude   float64
	Longitude  float64
	Mode       string // FIXED, LIVE or SPOT
	AccuracyM  *float64
	HeadingDeg *float64
	Moving     bool
	RecordedAt *time.Time
}

// Outlet is the platform's copy of one POS outlet.
type Outlet struct {
	ID              string
	MerchantID      string
	MerchantName    string
	POSOutletID     string
	Name            string
	Address         *string
	Phone           *string
	BaseLat         *float64
	BaseLng         *float64
	LocationMode    string
	LocationSharing string
	Timezone        string
	OnlineStatus    string
	PrepTimeMinutes int
	PricingValid    bool
	Active          bool
	Published       bool
	// OnlinePayment is whether the merchant has a payment gateway customers can pay with.
	OnlinePayment  bool
	StateVersion   int
	CatalogVersion int
	Hours          []Hours
	Session        Session
	Current        Current
	Pos            *Position
}

// out reports whether the outlet is out today: a mobile vendor only while its day is open.
func (o *Outlet) out() bool {
	return o.LocationMode != ModeMobile || o.Session.Active
}

// IsOpenNow mirrors the POS: closed outlets and mobile vendors without an open day are not open.
func (o *Outlet) IsOpenNow(now time.Time) bool {
	return o.Active && o.out() && openByStatus(o.OnlineStatus, o.Hours, now, o.Timezone)
}

// AcceptingOrders mirrors the POS: open, priced, and not paused.
func (o *Outlet) AcceptingOrders(now time.Time) bool {
	if !o.out() || !o.Active || !o.PricingValid || o.OnlineStatus == StatusPaused {
		return false
	}

	return openByStatus(o.OnlineStatus, o.Hours, now, o.Timezone)
}

func openByStatus(status string, hours []Hours, now time.Time, timezone string) bool {
	switch status {
	case StatusClosed:
		return false
	case StatusOpen, StatusPaused:
		return true
	default:
		return WithinHours(hours, now, timezone)
	}
}

// DerivePosition decides where customers see the outlet, exactly as the POS presents it:
// a fixed outlet at its address; a mobile one only during an open day, as the live dot,
// the chosen spot, or nowhere (HIDDEN).
func (o *Outlet) DerivePosition() *Position {
	if o.LocationMode != ModeMobile {
		if o.BaseLat == nil || o.BaseLng == nil {
			return nil
		}

		return &Position{Latitude: *o.BaseLat, Longitude: *o.BaseLng, Mode: ModeFixed}
	}

	if !o.Session.Active || o.Session.Sharing == SharingHidden {
		return nil
	}

	cur := o.Current
	if o.Session.Sharing == SharingLive && cur.Latitude != nil && cur.Longitude != nil {
		return &Position{
			Latitude: *cur.Latitude, Longitude: *cur.Longitude, Mode: SharingLive,
			AccuracyM: cur.AccuracyM, HeadingDeg: cur.HeadingDeg, Moving: cur.Moving, RecordedAt: cur.RecordedAt,
		}
	}

	if s := o.Session.Spot; s != nil && s.Latitude != nil && s.Longitude != nil {
		return &Position{Latitude: *s.Latitude, Longitude: *s.Longitude, Mode: SharingSpot}
	}

	return nil
}

// Freshness classifies a live position: "fresh", "stale" (show "last seen") or "hidden".
// Fixed places and spots never go stale.
func (p *Position) Freshness(now time.Time) string {
	if p == nil || p.Mode != SharingLive || p.RecordedAt == nil {
		return "fresh"
	}

	age := now.Sub(*p.RecordedAt)

	switch {
	case age > HideAfter:
		return "hidden"
	case age > StaleAfter:
		return "stale"
	default:
		return "fresh"
	}
}
