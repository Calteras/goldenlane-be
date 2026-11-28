package entity

import (
	"encoding/json"
	"errors"
	"time"
)

// Errors shared by the layers.
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrUnauthorized = errors.New("unauthorized")
	ErrBadRequest   = errors.New("bad request")
	ErrForbidden    = errors.New("forbidden")
	// ErrOutletUnknown means an event refers to an outlet we have not learned yet.
	// It is retried: the outlet's own event or the next reconciliation brings it.
	ErrOutletUnknown = errors.New("outlet not known yet")
	// ErrUnregistered means a push token is no longer valid and should be forgotten.
	ErrUnregistered = errors.New("push token is not registered")
	// ErrIdempotency means an idempotency key was reused for a different request.
	ErrIdempotency = errors.New("idempotency key reused with a different request")
	// ErrTooMany means the customer hit an ordering limit.
	ErrTooMany = errors.New("too many orders")
	// ErrCannotCancel means the order is past the point where the customer can cancel it.
	ErrCannotCancel = errors.New("order cannot be cancelled any more")
	// ErrNotPayable means the order can no longer be paid: it is paid, closed or past its deadline.
	ErrNotPayable = errors.New("order cannot be paid any more")
)

// Merchant is a business linked to a POS.
type Merchant struct {
	ID          string    `json:"id"`
	POSTenantID string    `json:"posTenantId"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	KeyID       string    `json:"keyId"`
	POSBaseURL  string    `json:"posBaseUrl"`
	LinkStatus  string    `json:"linkStatus"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Link is how to reach and verify one merchant's POS.
type Link struct {
	MerchantID  string
	POSTenantID string
	KeyID       string
	SecretEnc   string
	BaseURL     string
	Status      string
}

// InboxEvent is a POS event as received.
type InboxEvent struct {
	EventID    string
	Type       string
	OutletID   *string
	Version    int
	OccurredAt time.Time
	Payload    json.RawMessage
}

// InboxRow is a stored event being processed.
type InboxRow struct {
	ID         string
	KeyID      string
	EventID    string
	Type       string
	OutletID   *string
	Version    int
	OccurredAt time.Time
	Payload    json.RawMessage
	Attempts   int
}

// POSOutlet is the outlet object the POS serves and publishes (contract section 3.2).
type POSOutlet struct {
	OutletID        string   `json:"outletId"`
	Active          bool     `json:"active"`
	Name            string   `json:"name"`
	Address         *string  `json:"address"`
	Phone           *string  `json:"phone"`
	Latitude        *float64 `json:"latitude"`
	Longitude       *float64 `json:"longitude"`
	LocationMode    string   `json:"locationMode"`
	LocationSharing string   `json:"locationSharing"`
	Timezone        string   `json:"timezone"`
	OnlineStatus    string   `json:"onlineStatus"`
	PrepTimeMinutes int      `json:"prepTimeMinutes"`
	PricingValid    bool     `json:"pricingValid"`
	Hours           []Hours  `json:"hours"`
	StateVersion    int      `json:"stateVersion"`

	// Present only in list and snapshot responses, not in events.
	Session  *POSSession  `json:"session"`
	Location *POSLocation `json:"location"`
}

// Anchor is the place a working day is held, named or not.
type Anchor struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// POSSession is a working day as the POS reports it.
type POSSession struct {
	Active    bool       `json:"active"`
	SessionID string     `json:"sessionId"`
	StartedAt *time.Time `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt"`
	Sharing   string     `json:"sharing"`
	Spot      *Spot      `json:"spot"`
	Anchor    *Anchor    `json:"anchor"`
}

// Place is where the session is held: the saved spot if there is one, else the anchor
// (an unnamed place has an empty name). Nil when the POS did not say.
func Place(spot *Spot, anchor *Anchor) *Spot {
	if spot != nil && spot.Latitude != nil && spot.Longitude != nil {
		return spot
	}

	if anchor != nil {
		lat, lng := anchor.Latitude, anchor.Longitude

		return &Spot{Latitude: &lat, Longitude: &lng}
	}

	return spot
}

// POSLocation is a position as the POS reports it (events and snapshots).
type POSLocation struct {
	Mode       string     `json:"mode"`
	Latitude   float64    `json:"latitude"`
	Longitude  float64    `json:"longitude"`
	AccuracyM  *float64   `json:"accuracyM"`
	HeadingDeg *float64   `json:"headingDeg"`
	SpeedMps   *float64   `json:"speedMps"`
	Moving     bool       `json:"moving"`
	RecordedAt *time.Time `json:"recordedAt"`
	SessionID  string     `json:"sessionId"`
}

// POSSnapshot is one outlet with its catalog. The catalog is kept raw, as the POS
// served it, because the app needs variants and options we do not model.
type POSSnapshot struct {
	GeneratedAt time.Time       `json:"generatedAt"`
	Outlet      POSOutlet       `json:"outlet"`
	Catalog     json.RawMessage `json:"catalog"`
}

// POSCatalog is the part of the catalog the platform reads itself.
type POSCatalog struct {
	CatalogVersion int           `json:"catalogVersion"`
	Categories     []POSCategory `json:"categories"`
}

// POSCategory is a menu section.
type POSCategory struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Products []POSProduct `json:"products"`
}

// POSProduct is a menu item.
type POSProduct struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	PriceMinor int64  `json:"priceMinor"`
	SoldOut    bool   `json:"soldOut"`
}

// ParseCatalog reads the version and the flat list of items out of a raw catalog.
func ParseCatalog(raw json.RawMessage) (int, []MenuItem, error) {
	var c POSCatalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return 0, nil, err
	}

	var items []MenuItem

	for _, cat := range c.Categories {
		for _, p := range cat.Products {
			items = append(items, MenuItem{
				POSProductID: p.ID, Name: p.Name, Category: cat.Name,
				PriceMinor: p.PriceMinor, SoldOut: p.SoldOut,
			})
		}
	}

	return c.CatalogVersion, items, nil
}

// MenuItem is a flattened menu entry for search.
type MenuItem struct {
	POSProductID string
	Name         string
	Category     string
	PriceMinor   int64
	SoldOut      bool
}

// Customer is an app user.
type Customer struct {
	ID        string    `json:"id"`
	Phone     *string   `json:"phone"`
	Email     *string   `json:"email"`
	Name      string    `json:"name"`
	AvatarURL *string   `json:"avatarUrl"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"createdAt"`
}

// SignInProfile is what a verified sign-in says about its user. Every field is optional:
// phone sign-in has no email, and Apple may hide the name.
type SignInProfile struct {
	Phone     string
	Email     string // only a verified address
	Name      string
	AvatarURL string
}

// Menu is a stored snapshot of an outlet's menu.
type Menu struct {
	CatalogVersion int             `json:"catalogVersion"`
	FetchedAt      time.Time       `json:"fetchedAt"`
	Payload        json.RawMessage `json:"-"`
}
