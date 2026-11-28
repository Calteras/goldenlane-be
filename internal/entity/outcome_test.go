package entity_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

func TestNormalizeClose(t *testing.T) {
	for _, tc := range []struct {
		name, status, code, by, wantCode, wantBy string
	}{
		{"an older POS says nothing about a cancel", "CANCELLED", "", "", "OTHER", "VENDOR"},
		{"an older POS says nothing about an expiry", "EXPIRED", "", "", "ACCEPT_TIMEOUT", "SYSTEM"},
		{"a customer cancel without a code", "CANCELLED", "", "CUSTOMER", "CUSTOMER_CANCELLED", "CUSTOMER"},
		{"a code without who", "CANCELLED", "NO_SHOW", "", "NO_SHOW", "VENDOR"},
		{"both given are kept", "REJECTED", "OUT_OF_STOCK", "VENDOR", "OUT_OF_STOCK", "VENDOR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, by := entity.NormalizeClose(tc.status, tc.code, tc.by)
			require.Equal(t, tc.wantCode, code)
			require.Equal(t, tc.wantBy, by)
		})
	}
}

func outcome(status, code string, readyAgo time.Duration) entity.Outcome {
	now := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)
	o := entity.Outcome{
		CustomerID: "c1", OrderID: "o1", OutletID: "ou1", MerchantID: "m1",
		Status: status, ReasonCode: code, PaymentMode: entity.PaymentModePayAtPick, Now: now,
	}

	if readyAgo > 0 {
		ready := now.Add(-readyAgo)
		o.ReadyAt = &ready
	}

	return o
}

func evidenceOf(t *testing.T, ev entity.TrustEvent) map[string]any {
	t.Helper()

	var m map[string]any
	require.NoError(t, json.Unmarshal(ev.Evidence, &m))

	return m
}

func TestOutcomeEvents(t *testing.T) {
	t.Run("a no-show after the grace period is corroborated", func(t *testing.T) {
		evs := entity.OutcomeEvents(outcome("CANCELLED", "NO_SHOW", 25*time.Minute))
		require.Len(t, evs, 1)
		require.Equal(t, entity.EventNoShow, evs[0].Type)
		require.Equal(t, entity.SourceVendor, evs[0].Source)

		ev := evidenceOf(t, evs[0])
		require.Equal(t, true, ev["corroborated"])
		require.EqualValues(t, 25, ev["waitedMinutes"])
		require.EqualValues(t, 20, ev["graceMinutes"])
	})

	t.Run("a no-show inside the grace period is not", func(t *testing.T) {
		evs := entity.OutcomeEvents(outcome("CANCELLED", "NO_SHOW", 5*time.Minute))
		require.Equal(t, false, evidenceOf(t, evs[0])["corroborated"])
	})

	t.Run("a no-show for an order this platform never saw ready is not", func(t *testing.T) {
		o := outcome("CANCELLED", "NO_SHOW", 0)
		claim := 40
		o.VendorWaitedMinutes = &claim

		ev := evidenceOf(t, entity.OutcomeEvents(o)[0])
		require.Equal(t, false, ev["corroborated"])
		require.EqualValues(t, 40, ev["vendorWaitedMinutes"], "the claim is kept as evidence, not trusted")
	})

	t.Run("each ending says what it was", func(t *testing.T) {
		for code, want := range map[string]string{
			"CUSTOMER_CANCELLED":   entity.EventCancelBeforeAcc,
			"CUSTOMER_REQUEST":     entity.EventCancelAfterAccept,
			"PAYMENT_REFUSED":      entity.EventPaymentRefused,
			"CUSTOMER_UNREACHABLE": entity.EventUnreachable,
			"ABUSIVE_CUSTOMER":     entity.EventAbusiveReported,
			// The vendor's own reasons never count against the customer.
			"OUT_OF_STOCK":       entity.EventVendorCancelled,
			"VENDOR_CLOSED":      entity.EventVendorCancelled,
			"VENDOR_NOT_AT_SPOT": entity.EventVendorCancelled,
			"OTHER":              entity.EventVendorCancelled,
		} {
			evs := entity.OutcomeEvents(outcome("CANCELLED", code, 0))
			require.Len(t, evs, 1, code)
			require.Equal(t, want, evs[0].Type, code)
		}
	})

	t.Run("a completed order is a good sign, a very late one is also noted", func(t *testing.T) {
		evs := entity.OutcomeEvents(outcome("COMPLETED", "", 10*time.Minute))
		require.Len(t, evs, 1)
		require.Equal(t, entity.EventOrderCompleted, evs[0].Type)

		late := entity.OutcomeEvents(outcome("COMPLETED", "", 45*time.Minute))
		require.Len(t, late, 2)
		require.Equal(t, entity.EventLatePickup, late[1].Type)
	})

	t.Run("a vendor rejection or silence is about the vendor", func(t *testing.T) {
		require.Equal(t, entity.EventVendorRejected, entity.OutcomeEvents(outcome("REJECTED", "OUT_OF_STOCK", 0))[0].Type)
		require.Equal(t, entity.EventVendorNoResponse, entity.OutcomeEvents(outcome("EXPIRED", "", 0))[0].Type)
	})

	t.Run("open states record nothing", func(t *testing.T) {
		for _, status := range []string{"PENDING_ACCEPTANCE", "ACCEPTED", "PREPARING", "READY"} {
			require.Empty(t, entity.OutcomeEvents(outcome(status, "", 0)), status)
		}
	})

	t.Run("the same outcome always gives the same events", func(t *testing.T) {
		a := entity.OutcomeEvents(outcome("CANCELLED", "NO_SHOW", 30*time.Minute))
		b := entity.OutcomeEvents(outcome("CANCELLED", "NO_SHOW", 30*time.Minute))
		require.Equal(t, a, b)
	})
}
