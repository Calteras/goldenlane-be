package entity

import (
	"encoding/json"
	"time"
)

// Why an order ended, as the POS reports it (contract: reasonCode).
const (
	ReasonNoShow           = "NO_SHOW"
	ReasonCustomerRequest  = "CUSTOMER_REQUEST"
	ReasonCustomerCancel   = "CUSTOMER_CANCELLED"
	ReasonUnreachable      = "CUSTOMER_UNREACHABLE"
	ReasonPaymentRefused   = "PAYMENT_REFUSED"
	ReasonAbusive          = "ABUSIVE_CUSTOMER"
	ReasonOutOfStock       = "OUT_OF_STOCK"
	ReasonVendorClosed     = "VENDOR_CLOSED"
	ReasonVendorNotAtSpot  = "VENDOR_NOT_AT_SPOT"
	ReasonAcceptTimeout    = "ACCEPT_TIMEOUT"
	ReasonPaymentExpired   = "PAYMENT_EXPIRED"
	ReasonOther            = "OTHER"
	CancelledByCustomer    = "CUSTOMER"
	CancelledByVendor      = "VENDOR"
	CancelledBySystem      = "SYSTEM"
	PickupViaCode          = "CODE"
	PickupViaManual        = "MANUAL"
	PickupCodeLength       = 4
	NoShowGrace            = 20 * time.Minute // the vendor waits this long after "ready" before a no-show
	LatePickupAfter        = 30 * time.Minute // a completed pickup later than this is "late"
	PickupCodeMaxOpenTries = 25
)

// Trust event types: facts about how a customer behaved.
const (
	EventOrderCompleted    = "ORDER_COMPLETED"
	EventNoShow            = "NO_SHOW"
	EventLatePickup        = "LATE_PICKUP"
	EventCancelBeforeAcc   = "CANCEL_BEFORE_ACCEPT"
	EventCancelUnpaid      = "CANCEL_UNPAID"
	EventCancelAfterAccept = "CANCEL_AFTER_ACCEPT"
	EventPaymentRefused    = "PAYMENT_REFUSED"
	EventPaymentExpired    = "PAYMENT_EXPIRED"
	EventPriceChanged      = "PRICE_CHANGED_REFUSAL"
	EventUnreachable       = "UNREACHABLE"
	EventAbusiveReported   = "ABUSIVE_REPORTED"
	EventOpenCapHit        = "OPEN_CAP_HIT"
	// A vendor's one-tap rating of a customer after the order is over.
	EventVendorThumbsUp   = "VENDOR_THUMBS_UP"
	EventVendorThumbsDown = "VENDOR_THUMBS_DOWN"
	// Facts about the vendor's side. They never count against the customer; they exist so a
	// vendor who cancels or rejects a lot can be spotted.
	EventVendorCancelled  = "VENDOR_CANCELLED"
	EventVendorRejected   = "VENDOR_REJECTED"
	EventVendorNoResponse = "VENDOR_NO_RESPONSE"
)

// Who says an event happened.
const (
	SourceSystem   = "SYSTEM"
	SourceVendor   = "VENDOR"
	SourceGateway  = "GATEWAY"
	SourceOperator = "OPERATOR"
	SourceBackfill = "BACKFILL"
)

// Trust tiers, from the least to the most limited.
const (
	TierNew        = "NEW"
	TierGood       = "GOOD"
	TierWatch      = "WATCH"
	TierRestricted = "RESTRICTED"
	TierSuspended  = "SUSPENDED"
)

// TrustEvent is one recorded fact about a customer.
type TrustEvent struct {
	ID         int64           `json:"id"`
	CustomerID string          `json:"customerId"`
	Type       string          `json:"type"`
	Source     string          `json:"source"`
	OrderID    *string         `json:"orderId"`
	OutletID   *string         `json:"outletId"`
	MerchantID *string         `json:"merchantId"`
	Evidence   json.RawMessage `json:"evidence"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// TrustReason is one line of why a customer has the score they have.
type TrustReason struct {
	Code   string  `json:"code"`
	Count  int     `json:"count"`
	Points float64 `json:"points"`
}

// TrustProfile is where a customer stands. It is computed from their events.
type TrustProfile struct {
	CustomerID      string        `json:"customerId"`
	Score           float64       `json:"score"`
	Tier            string        `json:"tier"`
	FinishedOrders  int           `json:"finishedOrders"`
	CompletedOrders int           `json:"completedOrders"`
	Reasons         []TrustReason `json:"reasons"`
	ComputedAt      time.Time     `json:"computedAt"`
}

// What a vendor is told about a customer. A label, never a score.
const (
	LabelNew            = "NEW"
	LabelReliable       = "RELIABLE"
	LabelCaution        = "CAUTION"
	LabelPrepayRequired = "PREPAY_REQUIRED"
)

// RatingWindow is how long after an order ends a vendor can still rate the customer.
const RatingWindow = 7 * 24 * time.Hour

// VendorTrust is what travels to the POS with an order: a label and facts that are safe to show
// the vendor. No score, and nothing about other vendors.
type VendorTrust struct {
	Label             string `json:"label"`
	CompletedOrders   int    `json:"completedOrders"`
	NoShowsLast90Days int    `json:"noShowsLast90Days"`
}

// LabelFor is the label of a tier.
func LabelFor(tier string) string {
	switch tier {
	case TierGood:
		return LabelReliable
	case TierWatch:
		return LabelCaution
	case TierRestricted, TierSuspended:
		return LabelPrepayRequired
	default:
		return LabelNew
	}
}

// VendorStat is how one merchant's orders ended over a period, from the trust events.
type VendorStat struct {
	MerchantID       string   `json:"merchantId"`
	Name             string   `json:"name"`
	Completed        int      `json:"completed"`
	NoShows          int      `json:"noShows"`
	VendorCancelled  int      `json:"vendorCancelled"`
	VendorRejected   int      `json:"vendorRejected"`
	VendorNoResponse int      `json:"vendorNoResponse"`
	ThumbsDown       int      `json:"thumbsDown"`
	ThumbsUp         int      `json:"thumbsUp"`
	NoShowRate       float64  `json:"noShowRate"`
	CancelRate       float64  `json:"cancelRate"`
	RejectRate       float64  `json:"rejectRate"`
	DecidedOrders    int      `json:"decidedOrders"`
	Flags            []string `json:"flags"`
}
