// Package trust turns the facts recorded about a customer into a score, a tier and the reasons
// behind them. The rules are plain and deterministic: the same events always give the same
// answer, and every point can be traced to an event. Nothing here blocks anyone yet (shadow mode).
// See docs/CUSTOMER_TRUST_AND_SAFETY_PLAN.md.
package trust

import (
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// Weight is how much one kind of event counts, and how fast it fades.
type Weight struct {
	// Points is added to the score. Negative for good behaviour.
	Points float64 `json:"points"`
	// HalfLifeDays is how long until the event counts for half. Zero means it never fades.
	HalfLifeDays float64 `json:"halfLifeDays"`
}

// Config holds every number the rules use. Defaults are below; the trust_config table can
// override them without a deploy.
type Config struct {
	Weights map[string]Weight `json:"weights"`
	// PriorOrders is how many neutral orders every customer starts with: a customer with one
	// order and one problem is not judged like one with fifty orders and one problem.
	PriorOrders float64 `json:"priorOrders"`
	// NewBelow is the number of finished orders under which a customer is simply NEW.
	NewBelow int `json:"newBelow"`
	// WatchAt and RestrictedAt are the score thresholds of the WATCH and RESTRICTED tiers.
	WatchAt      float64 `json:"watchAt"`
	RestrictedAt float64 `json:"restrictedAt"`
	// VendorCap is the most one merchant can add to a customer's score within the window, so a
	// single unfair vendor cannot sink anyone.
	VendorCap float64 `json:"vendorCap"`
	// UncorroboratedFactor scales a vendor's claim that the platform's own facts do not back up.
	UncorroboratedFactor float64 `json:"uncorroboratedFactor"`
	// WindowDays is how far back events are read.
	WindowDays int `json:"windowDays"`
	// EarlyCancelMin and EarlyCancelRatio: early cancels only count once a customer does it
	// often (at least this many in 30 days, and at least this share of their orders).
	EarlyCancelMin   int     `json:"earlyCancelMin"`
	EarlyCancelRatio float64 `json:"earlyCancelRatio"`
}

// NoShowPrepaid is the weight key of a no-show on an order that was paid in the app.
const NoShowPrepaid = "NO_SHOW_PREPAID"

// DefaultConfig is where the weights start. They are a first guess, to be tuned from shadow data.
func DefaultConfig() Config {
	return Config{
		Weights: map[string]Weight{
			entity.EventNoShow:            {Points: 30, HalfLifeDays: 60},
			NoShowPrepaid:                 {Points: 20, HalfLifeDays: 60},
			entity.EventCancelAfterAccept: {Points: 10, HalfLifeDays: 30},
			entity.EventPaymentRefused:    {Points: 35, HalfLifeDays: 90},
			entity.EventPaymentExpired:    {Points: 4, HalfLifeDays: 14},
			entity.EventCancelBeforeAcc:   {Points: 1, HalfLifeDays: 14},
			entity.EventPriceChanged:      {Points: 0.5, HalfLifeDays: 7},
			entity.EventLatePickup:        {Points: 2, HalfLifeDays: 14},
			entity.EventUnreachable:       {Points: 8, HalfLifeDays: 45},
			entity.EventAbusiveReported:   {Points: 15, HalfLifeDays: 90},
			entity.EventOpenCapHit:        {Points: 1, HalfLifeDays: 7},
			entity.EventOrderCompleted:    {Points: -6, HalfLifeDays: 90},
		},
		PriorOrders: 3, NewBelow: 3, WatchAt: 20, RestrictedAt: 50,
		VendorCap: 60, UncorroboratedFactor: 0.5, WindowDays: 365,
		EarlyCancelMin: 3, EarlyCancelRatio: 0.5,
	}
}

// finishes are the events that end an order for a reason that is about the customer, the
// denominator of "how often".
var finishes = map[string]bool{
	entity.EventOrderCompleted: true, entity.EventNoShow: true,
	entity.EventCancelAfterAccept: true, entity.EventPaymentRefused: true,
}

// Result is the outcome of scoring one customer.
type Result struct {
	Score     float64
	Tier      string
	Finished  int
	Completed int
	Reasons   []entity.TrustReason
}

type evidence struct {
	Corroborated *bool  `json:"corroborated"`
	PaymentMode  string `json:"paymentMode"`
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// Score computes where a customer stands at now, from their events.
func Score(events []entity.TrustEvent, cfg Config, now time.Time) Result {
	windowStart := now.AddDate(0, 0, -cfg.WindowDays)
	monthStart := now.AddDate(0, 0, -30)

	var (
		res         Result
		earlyMonth  int
		ordersMonth int
	)

	// First pass: how many orders and early cancels in the last 30 days, for the early cancel rule.
	for _, ev := range events {
		if ev.CreatedAt.Before(monthStart) {
			continue
		}

		switch {
		case ev.Type == entity.EventCancelBeforeAcc:
			earlyMonth++
			ordersMonth++
		case finishes[ev.Type]:
			ordersMonth++
		}
	}

	earlyCancelsCount := earlyMonth >= cfg.EarlyCancelMin && float64(earlyMonth) >= cfg.EarlyCancelRatio*float64(ordersMonth)

	type line struct {
		points   float64
		merchant string
		vendor   bool
	}

	lines := map[string][]line{}

	for _, ev := range events {
		if ev.CreatedAt.Before(windowStart) {
			continue
		}

		if finishes[ev.Type] {
			res.Finished++
		}

		if ev.Type == entity.EventOrderCompleted {
			res.Completed++
		}

		var e evidence
		_ = json.Unmarshal(ev.Evidence, &e)

		key := ev.Type
		if ev.Type == entity.EventNoShow && e.PaymentMode == entity.PaymentModeOnline {
			key = NoShowPrepaid
		}

		w, known := cfg.Weights[key]
		if !known || w.Points == 0 {
			continue
		}

		if ev.Type == entity.EventCancelBeforeAcc && !earlyCancelsCount {
			continue
		}

		points := w.Points

		if w.HalfLifeDays > 0 {
			age := now.Sub(ev.CreatedAt).Hours() / 24
			points *= math.Pow(0.5, math.Max(0, age)/w.HalfLifeDays)
		}

		// A vendor's claim the platform's own facts do not back up counts for less.
		if ev.Source == entity.SourceVendor && e.Corroborated != nil && !*e.Corroborated {
			points *= cfg.UncorroboratedFactor
		}

		merchant := ""
		if ev.MerchantID != nil {
			merchant = *ev.MerchantID
		}

		lines[ev.Type] = append(lines[ev.Type], line{points: points, merchant: merchant, vendor: ev.Source == entity.SourceVendor && points > 0})
	}

	// Walk the kinds in a fixed order, so the sums are the same every time.
	kinds := make([]string, 0, len(lines))
	for kind := range lines {
		kinds = append(kinds, kind)
	}

	sort.Strings(kinds)

	// One merchant can add only so much, however many things it reports.
	perMerchant := map[string]float64{}

	for _, kind := range kinds {
		for _, l := range lines[kind] {
			if l.vendor && l.merchant != "" {
				perMerchant[l.merchant] += l.points
			}
		}
	}

	scale := func(l line) float64 {
		if l.vendor && l.merchant != "" && perMerchant[l.merchant] > cfg.VendorCap {
			return l.points * cfg.VendorCap / perMerchant[l.merchant]
		}

		return l.points
	}

	var raw float64

	for _, kind := range kinds {
		ls := lines[kind]

		var sum float64
		for _, l := range ls {
			sum += scale(l)
		}

		raw += sum
		res.Reasons = append(res.Reasons, entity.TrustReason{Code: kind, Count: len(ls), Points: round2(sum)})
	}

	sort.Slice(res.Reasons, func(i, j int) bool {
		a, b := math.Abs(res.Reasons[i].Points), math.Abs(res.Reasons[j].Points)
		if a != b {
			return a > b
		}

		return res.Reasons[i].Code < res.Reasons[j].Code
	})

	// Smoothing: the prior of neutral orders makes a short history count for less.
	finished := float64(res.Finished)
	smoothed := math.Max(0, raw) * finished / (finished + cfg.PriorOrders)
	res.Score = round2(smoothed)
	res.Tier = tierOf(res.Score, res.Finished, cfg)

	return res
}

// tierOf is the tier a score earns. SUSPENDED is never computed: only a person can suspend.
func tierOf(score float64, finished int, cfg Config) string {
	switch {
	case finished < cfg.NewBelow:
		return entity.TierNew
	case score >= cfg.RestrictedAt:
		return entity.TierRestricted
	case score >= cfg.WatchAt:
		return entity.TierWatch
	default:
		return entity.TierGood
	}
}
