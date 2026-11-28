package entity

import (
	"encoding/json"
	"time"
)

// Order statuses. The first two exist only on the platform; the rest mirror the POS.
const (
	OrderUnpaid          = "UNPAID"        // waiting for the customer to pay online
	OrderCreated         = "CREATED"       // stored, not yet accepted by the POS
	OrderSubmitFailed    = "SUBMIT_FAILED" // the POS refused it, or could not be reached in time
	OrderPendingAccept   = "PENDING_ACCEPTANCE"
	OrderAccepted        = "ACCEPTED"
	OrderPreparing       = "PREPARING"
	OrderReady           = "READY"
	OrderCompleted       = "COMPLETED"
	OrderRejected        = "REJECTED"
	OrderCancelled       = "CANCELLED"
	OrderExpired         = "EXPIRED"
	FulfillmentPickup    = "PICKUP"
	FulfillmentDineIn    = "DINE_IN"
	PaymentModePayAtPick = "PAY_AT_PICKUP"
	PaymentModeOnline    = "ONLINE"
)

// Limits that keep one account from flooding merchants.
const (
	MaxOpenOrders       = 5
	MaxCheckoutsPerHour = 20
	MaxItemsPerOrder    = 50
	MaxLinesPerOrder    = 30
)

// IsOpen reports whether the order is still going on: the customer can expect news.
func IsOpen(status string) bool {
	switch status {
	case OrderUnpaid, OrderCreated, OrderPendingAccept, OrderAccepted, OrderPreparing, OrderReady:
		return true
	default:
		return false
	}
}

// OrderItem is one line of an order as the customer chose it.
type OrderItem struct {
	LineNo      int      `json:"lineNo"`
	ProductID   string   `json:"productId"`
	VariantID   *string  `json:"variantId"`
	ModifierIDs []string `json:"modifierIds"`
	Quantity    int      `json:"quantity"`
	Notes       *string  `json:"notes"`
	Name        string   `json:"name"`
	VariantName *string  `json:"variantName"`
}

// Order is a customer's order at one merchant.
type Order struct {
	ID              string
	CheckoutID      string
	CustomerID      string
	MerchantID      string
	OutletID        string
	OutletName      string
	PlatformOrderID string
	POSOrderID      *string
	Status          string
	POSVersion      int
	FulfillmentType string
	FulfillmentLbl  *string
	Notes           *string
	PaymentMode     string
	SubtotalMinor   *int64
	TaxMinor        *int64
	ServiceMinor    *int64
	TotalMinor      *int64
	AcceptBy        *time.Time
	PrepMinutes     *int
	Reason          *string
	ErrorCode       *string
	ErrorDetail     json.RawMessage
	ExpectedTotal   *int64
	PaymentDeadline *time.Time
	// Payment is the latest payment attempt of an online order.
	Payment        *Payment
	SubmitAttempts int
	Items          []OrderItem
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
	// ReadyAt is when the platform learned the order was ready (its own clock).
	ReadyAt         *time.Time
	NoShowAt        *time.Time
	CancelledBy     *string
	CloseReasonCode *string
	// PickupCode is what the customer shows to find the order; PickupVia says how it was handed over.
	PickupCode        *string
	PickupConfirmedAt *time.Time
	PickupVia         *string
}

// LineProblem is why one line cannot be ordered, using the POS's own codes.
type LineProblem struct {
	Line  int    `json:"line"`
	Code  string `json:"code"`
	Group string `json:"group,omitempty"`
	Min   int    `json:"min,omitempty"`
	Max   int    `json:"max,omitempty"`
}

// Line problem codes (shared with the POS).
const (
	ProductUnavailable = "PRODUCT_UNAVAILABLE"
	VariantUnavailable = "VARIANT_UNAVAILABLE"
	OptionUnavailable  = "OPTION_UNAVAILABLE"
	OptionSelection    = "OPTION_SELECTION"
)

// MenuProduct is a product as the stored menu describes it.
type MenuProduct struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SoldOut  bool   `json:"soldOut"`
	Variants []struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Available bool   `json:"available"`
	} `json:"variants"`
	ModifierGroups []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		MinSelection int    `json:"minSelection"`
		MaxSelection int    `json:"maxSelection"`
		Options      []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			Available bool   `json:"available"`
		} `json:"options"`
	} `json:"modifierGroups"`
}

// MenuIndex answers "can the customer order this?" from the stored menu. It is an early,
// friendly check: the POS prices and decides, and may still refuse.
type MenuIndex map[string]MenuProduct

// ParseMenuIndex reads the stored menu payload.
func ParseMenuIndex(payload json.RawMessage) (MenuIndex, error) {
	var m struct {
		Categories []struct {
			Products []MenuProduct `json:"products"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}

	idx := MenuIndex{}

	for _, c := range m.Categories {
		for _, p := range c.Products {
			idx[p.ID] = p
		}
	}

	return idx, nil
}

// OrderLine is one line of a checkout request.
type OrderLine struct {
	ProductID   string
	VariantID   *string
	ModifierIDs []string
	Quantity    int
	Notes       *string
}

// Check validates lines against the menu and names them as the customer saw them. Problems
// use the POS's codes, so an app handles both the same way.
func (m MenuIndex) Check(lines []OrderLine) ([]OrderItem, []LineProblem) {
	items := make([]OrderItem, 0, len(lines))

	var problems []LineProblem

	for i, l := range lines {
		p, found := m[l.ProductID]
		if !found || p.SoldOut {
			problems = append(problems, LineProblem{Line: i, Code: ProductUnavailable})

			continue
		}

		item := OrderItem{
			LineNo: i + 1, ProductID: l.ProductID, VariantID: l.VariantID, ModifierIDs: append([]string{}, l.ModifierIDs...),
			Quantity: l.Quantity, Notes: l.Notes, Name: p.Name,
		}

		if l.VariantID != nil {
			var ok bool

			for _, v := range p.Variants {
				if v.ID == *l.VariantID {
					ok = v.Available
					name := v.Name
					item.VariantName = &name
				}
			}

			if !ok {
				problems = append(problems, LineProblem{Line: i, Code: VariantUnavailable})

				continue
			}
		}

		if prob := checkOptions(p, l.ModifierIDs); prob != nil {
			prob.Line = i
			problems = append(problems, *prob)

			continue
		}

		items = append(items, item)
	}

	return items, problems
}

func checkOptions(p MenuProduct, chosen []string) *LineProblem {
	seen := map[string]bool{}

	for _, id := range chosen {
		if seen[id] {
			return &LineProblem{Code: OptionUnavailable}
		}

		seen[id] = true
	}

	owned := map[string]bool{}

	for _, g := range p.ModifierGroups {
		count := 0

		for _, o := range g.Options {
			owned[o.ID] = true

			if seen[o.ID] {
				if !o.Available {
					return &LineProblem{Code: OptionUnavailable, Group: g.Name}
				}

				count++
			}
		}

		if count < g.MinSelection || count > g.MaxSelection {
			return &LineProblem{Code: OptionSelection, Group: g.Name, Min: g.MinSelection, Max: g.MaxSelection}
		}
	}

	for _, id := range chosen {
		if !owned[id] {
			return &LineProblem{Code: OptionUnavailable}
		}
	}

	return nil
}

// POSOrderState is what the POS says about an order.
type POSOrderState struct {
	POSOrderID  string
	Status      string
	Version     int
	Subtotal    *int64
	Tax         *int64
	Service     *int64
	Total       *int64
	AcceptBy    *time.Time
	PrepMinutes *int
	Reason      *string
	// What the POS says about how the order ended (empty from an older POS).
	ReasonCode    string
	CancelledBy   string
	PickupVia     string
	WaitedMinutes *int
	// ReadyAtPOS is the vendor's own claim of when the order was ready, kept as evidence.
	ReadyAtPOS *time.Time
}

// Notification is a push message for a customer.
type Notification struct {
	ID         string
	CustomerID string
	OrderID    string
	Kind       string
	Title      string
	Body       string
	Data       map[string]string
	Attempts   int
}

// Review is a customer's rating of a completed order.
type Review struct {
	ID        string    `json:"id"`
	OrderID   string    `json:"orderId,omitempty"`
	Rating    int       `json:"rating"`
	Comment   *string   `json:"comment"`
	Reviewer  string    `json:"reviewer"`
	CreatedAt time.Time `json:"createdAt"`
}

// Rating summarizes an outlet's reviews.
type Rating struct {
	Average float64 `json:"average"`
	Count   int     `json:"count"`
}

// Pricing is the POS's price of an order, in minor units.
type Pricing struct {
	SubtotalMinor int64
	TaxMinor      int64
	ServiceMinor  int64
	TotalMinor    int64
}
