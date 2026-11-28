package ordering_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/pos"
)

func posEvent(platformOrderID, status string, extra map[string]any) json.RawMessage {
	m := map[string]any{"platformOrderId": platformOrderID, "status": status, "reason": nil}
	for k, v := range extra {
		m[k] = v
	}

	raw, _ := json.Marshal(m)

	return raw
}

// readyAt makes an accepted order ready, then moves its ready time into the past, as if the
// customer had been waited for that long.
func (w *world) readyFor(o *entity.Order, waited time.Duration) {
	w.T.Helper()

	require.NoError(w.T, w.UC.ApplyEvent(w.Ctx, 2, posEvent(o.PlatformOrderID, "ACCEPTED", nil)))
	require.NoError(w.T, w.UC.ApplyEvent(w.Ctx, 3, posEvent(o.PlatformOrderID, "PREPARING", nil)))
	require.NoError(w.T, w.UC.ApplyEvent(w.Ctx, 4, posEvent(o.PlatformOrderID, "READY", nil)))

	_, err := w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET ready_at = now() - make_interval(secs => $2) WHERE id = $1`, o.ID, waited.Seconds())
	require.NoError(w.T, err)
}

type trustRow struct {
	Type     string
	Source   string
	Evidence map[string]any
}

func (w *world) trustEvents(customerID string) []trustRow {
	w.T.Helper()

	rows, err := w.Pool.Query(w.Ctx, `SELECT event_type, source, evidence FROM customer_trust_events WHERE customer_id = $1 ORDER BY id`, customerID)
	require.NoError(w.T, err)
	defer rows.Close()

	var out []trustRow

	for rows.Next() {
		var (
			r   trustRow
			raw []byte
		)

		require.NoError(w.T, rows.Scan(&r.Type, &r.Source, &raw))
		require.NoError(w.T, json.Unmarshal(raw, &r.Evidence))

		out = append(out, r)
	}

	require.NoError(w.T, rows.Err())

	return out
}

func TestANoShowIsRecordedWithItsEvidence(t *testing.T) {
	w := setup(t)
	o := w.place(w.Rina, w.request())
	w.readyFor(o, 25*time.Minute)

	noShow := posEvent(o.PlatformOrderID, "CANCELLED", map[string]any{
		"reason": "Customer did not pick up the order", "reasonCode": "NO_SHOW", "cancelledBy": "VENDOR",
		"evidence": map[string]any{"readyAt": "2026-11-28T09:00:00Z", "waitedMinutes": 25},
	})
	require.NoError(t, w.UC.ApplyEvent(w.Ctx, 5, noShow))

	got := w.reload(o)
	require.Equal(t, entity.OrderCancelled, got.Status)
	require.Equal(t, "NO_SHOW", *got.CloseReasonCode)
	require.Equal(t, "VENDOR", *got.CancelledBy)
	require.NotNil(t, got.NoShowAt)
	require.NotNil(t, got.ReadyAt, "the platform kept its own clock for when the order was ready")

	events := w.trustEvents(w.Rina.ID)
	require.Len(t, events, 1)
	require.Equal(t, entity.EventNoShow, events[0].Type)
	require.Equal(t, entity.SourceVendor, events[0].Source)
	require.Equal(t, true, events[0].Evidence["corroborated"], "25 minutes passed on this platform's own clock")
	require.EqualValues(t, 25, events[0].Evidence["vendorWaitedMinutes"])

	t.Run("a redelivery records nothing new", func(t *testing.T) {
		require.NoError(t, w.UC.ApplyEvent(w.Ctx, 5, noShow))
		require.NoError(t, w.UC.ApplyEvent(w.Ctx, 6, noShow))
		require.Len(t, w.trustEvents(w.Rina.ID), 1)
	})

	t.Run("the customer is told, in plain words", func(t *testing.T) {
		var title, body string

		require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT title, body FROM notification_outbox WHERE order_id = $1 AND kind = 'CANCELLED'`, o.ID).Scan(&title, &body))
		require.Equal(t, "Pesanan tidak diambil", title)
		require.Contains(t, body, "tidak diambil")
		require.NotContains(t, body, "dikembalikan", "a pay-at-pickup order has no payment to return")
	})
}

func TestANoShowRightAfterReadyIsNotCorroborated(t *testing.T) {
	w := setup(t)
	o := w.place(w.Rina, w.request())
	w.readyFor(o, 2*time.Minute)

	require.NoError(t, w.UC.ApplyEvent(w.Ctx, 5, posEvent(o.PlatformOrderID, "CANCELLED", map[string]any{"reasonCode": "NO_SHOW", "cancelledBy": "VENDOR"})))

	events := w.trustEvents(w.Rina.ID)
	require.Len(t, events, 1)
	require.Equal(t, false, events[0].Evidence["corroborated"], "the vendor did not wait the grace period")
}

func TestAVendorsOwnReasonsNeverCountAgainstTheCustomer(t *testing.T) {
	w := setup(t)

	for _, code := range []string{"OUT_OF_STOCK", "VENDOR_NOT_AT_SPOT"} {
		o := w.place(w.Rina, w.request())
		require.NoError(t, w.UC.ApplyEvent(w.Ctx, 2, posEvent(o.PlatformOrderID, "ACCEPTED", nil)))
		require.NoError(t, w.UC.ApplyEvent(w.Ctx, 3, posEvent(o.PlatformOrderID, "CANCELLED", map[string]any{"reasonCode": code, "cancelledBy": "VENDOR"})))
	}

	for _, ev := range w.trustEvents(w.Rina.ID) {
		require.Equal(t, entity.EventVendorCancelled, ev.Type)
	}
}

func TestAnOlderPOSStillGetsAStructuredClose(t *testing.T) {
	w := setup(t)
	o := w.place(w.Rina, w.request())

	// No reasonCode, no cancelledBy: what a POS from before this change sends.
	require.NoError(t, w.UC.ApplyEvent(w.Ctx, 2, statusEvent(o.PlatformOrderID, "CANCELLED")))

	got := w.reload(o)
	require.Equal(t, "OTHER", *got.CloseReasonCode)
	require.Equal(t, "VENDOR", *got.CancelledBy)
}

func TestACustomerCancelIsRecordedAsTheirs(t *testing.T) {
	w := setup(t)

	t.Run("while the vendor has not answered", func(t *testing.T) {
		o := w.place(w.Rina, w.request())
		_, err := w.UC.Cancel(w.Ctx, w.Rina.ID, o.ID)
		require.NoError(t, err)

		got := w.reload(o)
		require.Equal(t, "CUSTOMER", *got.CancelledBy)
		require.Equal(t, "CUSTOMER_CANCELLED", *got.CloseReasonCode)
		require.Equal(t, entity.EventCancelBeforeAcc, w.trustEvents(w.Rina.ID)[0].Type)
	})

	t.Run("before the order even reached the POS", func(t *testing.T) {
		w := setup(t)
		w.POS.Down = true

		o := w.place(w.Budi, w.request())
		require.Equal(t, entity.OrderCreated, o.Status)

		_, err := w.UC.Cancel(w.Ctx, w.Budi.ID, o.ID)
		require.NoError(t, err)

		require.Equal(t, "CUSTOMER_CANCELLED", *w.reload(o).CloseReasonCode)
		require.Equal(t, entity.EventCancelBeforeAcc, w.trustEvents(w.Budi.ID)[0].Type)
	})
}

func TestACompletedOrderIsAGoodSign(t *testing.T) {
	w := setup(t)
	o := w.place(w.Rina, w.request())
	w.readyFor(o, 10*time.Minute)

	require.NoError(t, w.UC.ApplyEvent(w.Ctx, 5, posEvent(o.PlatformOrderID, "COMPLETED", map[string]any{"pickupConfirmedVia": "CODE"})))

	got := w.reload(o)
	require.Equal(t, "CODE", *got.PickupVia, "found and handed over against the customer's code")
	require.NotNil(t, got.PickupConfirmedAt)

	events := w.trustEvents(w.Rina.ID)
	require.Len(t, events, 1)
	require.Equal(t, entity.EventOrderCompleted, events[0].Type)
	require.Equal(t, "CODE", events[0].Evidence["pickupVia"])
}

func TestAPriceChangeRefusalIsNoted(t *testing.T) {
	w := setup(t)
	w.POS.SubmitErr = &pos.Error{Status: 409, Code: "PRICE_CHANGED"}

	_, _, err := w.UC.Checkout(w.Ctx, w.Rina, key(), w.request())
	require.Error(t, err)

	events := w.trustEvents(w.Rina.ID)
	require.Len(t, events, 1)
	require.Equal(t, entity.EventPriceChanged, events[0].Type)
}

func TestHittingTheOrderLimitIsNotedOncePerTenMinutes(t *testing.T) {
	w := setup(t)

	for i := 0; i < entity.MaxOpenOrders; i++ {
		w.place(w.Rina, w.request())
	}

	for i := 0; i < 3; i++ {
		_, _, err := w.UC.Checkout(w.Ctx, w.Rina, key(), w.request())
		require.ErrorIs(t, err, entity.ErrTooMany)
	}

	hits := 0

	for _, ev := range w.trustEvents(w.Rina.ID) {
		if ev.Type == entity.EventOpenCapHit {
			hits++
		}
	}

	require.Equal(t, 1, hits)
}

func TestPickupCodes(t *testing.T) {
	w := setup(t)

	var open []*entity.Order

	for i := 0; i < 4; i++ {
		open = append(open, w.place(w.Rina, w.request()), w.place(w.Budi, w.request()))
	}

	seen := map[string]bool{}

	for _, o := range open {
		require.NotNil(t, o.PickupCode)
		require.Regexp(t, `^[0-9]{4}$`, *o.PickupCode)
		require.False(t, seen[*o.PickupCode], "two open orders of one outlet never share a code")

		seen[*o.PickupCode] = true
	}

	require.Equal(t, *open[0].PickupCode, w.POS.Submitted[0].PickupCode, "the POS gets the code the customer sees")

	t.Run("the database refuses a second open order with the same code", func(t *testing.T) {
		_, err := w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET pickup_code = $2 WHERE id = $1`, open[1].ID, *open[0].PickupCode)
		require.Error(t, err)
	})

	t.Run("a code is free again once its order is over", func(t *testing.T) {
		require.NoError(t, w.UC.ApplyEvent(w.Ctx, 2, statusEvent(open[0].PlatformOrderID, "REJECTED")))

		_, err := w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET pickup_code = $2 WHERE id = $1`, open[1].ID, *open[0].PickupCode)
		require.NoError(t, err)
	})
}
