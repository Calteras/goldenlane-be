package entity

import (
	"encoding/json"
	"time"
)

// Outcome is what is known about an order at the moment it changes state.
type Outcome struct {
	CustomerID string
	OrderID    string
	OutletID   string
	MerchantID string
	Status     string // the new status
	// ReasonCode and CancelledBy are as the POS reported them; empty from an older POS.
	ReasonCode  string
	CancelledBy string
	PickupVia   string
	PaymentMode string
	// ReadyAt is when this platform learned the order was ready. Nil when it never saw READY.
	ReadyAt *time.Time
	// VendorWaitedMinutes is the vendor's own claim, kept next to ours as evidence.
	VendorWaitedMinutes *int
	Now                 time.Time
}

// NormalizeClose fills in the reason code and who ended an order when the POS did not say
// (an older POS), so every closed order has both.
func NormalizeClose(status, reasonCode, cancelledBy string) (code, by string) {
	code, by = reasonCode, cancelledBy

	if by == "" {
		switch {
		case status == OrderExpired:
			by = CancelledBySystem
		case code == ReasonCustomerCancel:
			by = CancelledByCustomer
		default:
			by = CancelledByVendor
		}
	}

	if code == "" {
		switch {
		case status == OrderExpired:
			code = ReasonAcceptTimeout
		case by == CancelledByCustomer:
			code = ReasonCustomerCancel
		default:
			code = ReasonOther
		}
	}

	return code, by
}

func evidence(m map[string]any) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{}`)
	}

	return b
}

// OutcomeEvents turns an order's new state into the trust events it stands for. It is pure:
// the same outcome always gives the same events.
func OutcomeEvents(o Outcome) []TrustEvent {
	code, _ := NormalizeClose(o.Status, o.ReasonCode, o.CancelledBy)

	base := func(kind, source string, ev map[string]any) TrustEvent {
		orderID, outletID, merchantID := o.OrderID, o.OutletID, o.MerchantID

		return TrustEvent{
			CustomerID: o.CustomerID, Type: kind, Source: source,
			OrderID: &orderID, OutletID: &outletID, MerchantID: &merchantID,
			Evidence: evidence(ev), CreatedAt: o.Now,
		}
	}

	var waited *int

	if o.ReadyAt != nil {
		m := int(o.Now.Sub(*o.ReadyAt) / time.Minute)
		waited = &m
	}

	switch o.Status {
	case OrderCompleted:
		ev := map[string]any{"paymentMode": o.PaymentMode, "pickupVia": o.PickupVia}
		if waited != nil {
			ev["waitedMinutes"] = *waited
		}

		out := []TrustEvent{base(EventOrderCompleted, SourceSystem, ev)}
		if waited != nil && time.Duration(*waited)*time.Minute >= LatePickupAfter {
			out = append(out, base(EventLatePickup, SourceSystem, map[string]any{"waitedMinutes": *waited}))
		}

		return out
	case OrderRejected:
		return []TrustEvent{base(EventVendorRejected, SourceVendor, map[string]any{"reasonCode": code})}
	case OrderExpired:
		return []TrustEvent{base(EventVendorNoResponse, SourceSystem, map[string]any{"reasonCode": code})}
	case OrderCancelled:
		return cancelEvents(o, code, waited, base)
	default:
		return nil
	}
}

func cancelEvents(o Outcome, code string, waited *int, base func(kind, source string, ev map[string]any) TrustEvent) []TrustEvent {
	ev := map[string]any{"reasonCode": code, "paymentMode": o.PaymentMode}

	switch code {
	case ReasonNoShow:
		// A no-show is the vendor's word. It is corroborated only when this platform itself saw
		// the order become ready and the grace period really passed.
		corroborated := waited != nil && time.Duration(*waited)*time.Minute >= NoShowGrace
		ev["corroborated"] = corroborated
		ev["graceMinutes"] = int(NoShowGrace / time.Minute)

		if waited != nil {
			ev["waitedMinutes"] = *waited
		}

		if o.VendorWaitedMinutes != nil {
			ev["vendorWaitedMinutes"] = *o.VendorWaitedMinutes
		}

		return []TrustEvent{base(EventNoShow, SourceVendor, ev)}
	case ReasonCustomerCancel:
		return []TrustEvent{base(EventCancelBeforeAcc, SourceSystem, ev)}
	case ReasonCustomerRequest:
		return []TrustEvent{base(EventCancelAfterAccept, SourceVendor, ev)}
	case ReasonPaymentRefused:
		return []TrustEvent{base(EventPaymentRefused, SourceVendor, ev)}
	case ReasonUnreachable:
		return []TrustEvent{base(EventUnreachable, SourceVendor, ev)}
	case ReasonAbusive:
		return []TrustEvent{base(EventAbusiveReported, SourceVendor, ev)}
	default:
		// The vendor's own reasons (out of stock, closed, not at the spot, other).
		return []TrustEvent{base(EventVendorCancelled, SourceVendor, ev)}
	}
}
