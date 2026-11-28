package trust

import "github.com/Calteras/streetfood-customer-api/internal/entity"

// Policy is what a tier would be allowed to do. In shadow mode it is only logged: nothing
// reads it to refuse an order yet (sprint 4).
type Policy struct {
	MaxOpenOrders       int  `json:"maxOpenOrders"`
	MaxCheckoutsPerHour int  `json:"maxCheckoutsPerHour"`
	PayAtPickup         bool `json:"payAtPickup"`
	// NeedsVerifiedPhone: pay at pickup only once the customer has a verified phone.
	NeedsVerifiedPhone bool `json:"needsVerifiedPhone"`
	// VendorMayRequirePrepay: the vendor can ask this customer to pay in the app.
	VendorMayRequirePrepay bool `json:"vendorMayRequirePrepay"`
	Reminder               bool `json:"reminder"`
	ReviewsHeld            bool `json:"reviewsHeld"`
}

// PolicyFor is the policy table of the plan (section 4).
func PolicyFor(tier string) Policy {
	switch tier {
	case entity.TierNew:
		return Policy{MaxOpenOrders: 2, MaxCheckoutsPerHour: 6, PayAtPickup: true, NeedsVerifiedPhone: true}
	case entity.TierWatch:
		return Policy{MaxOpenOrders: 3, MaxCheckoutsPerHour: 10, PayAtPickup: true, VendorMayRequirePrepay: true, Reminder: true}
	case entity.TierRestricted:
		return Policy{MaxOpenOrders: 1, MaxCheckoutsPerHour: 3, Reminder: true, ReviewsHeld: true}
	case entity.TierSuspended:
		return Policy{}
	default: // GOOD
		return Policy{MaxOpenOrders: entity.MaxOpenOrders, MaxCheckoutsPerHour: entity.MaxCheckoutsPerHour, PayAtPickup: true}
	}
}

// Differences lists where a tier's policy is stricter than what applies today, which is what
// shadow mode reports: the checkouts that would have been treated differently.
func Differences(p Policy, openOrders int, phoneVerified bool) []string {
	var out []string

	if p.MaxOpenOrders < entity.MaxOpenOrders && openOrders >= p.MaxOpenOrders {
		out = append(out, "OPEN_ORDER_CAP")
	}

	if !p.PayAtPickup {
		out = append(out, "PREPAY_REQUIRED")
	}

	if p.NeedsVerifiedPhone && !phoneVerified {
		out = append(out, "PHONE_REQUIRED")
	}

	if p.Reminder {
		out = append(out, "REMINDER")
	}

	if p.VendorMayRequirePrepay {
		out = append(out, "VENDOR_MAY_REQUIRE_PREPAY")
	}

	return out
}
