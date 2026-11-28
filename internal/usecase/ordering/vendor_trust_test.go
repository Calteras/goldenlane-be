package ordering_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/pos"
	"github.com/Calteras/streetfood-customer-api/internal/testutil"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/ordering"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/trust"
)

// seed gives a customer a history, without going through whole orders.
func (w *world) seed(c entity.Customer, kind, source string, daysAgo int, evidence string) {
	w.T.Helper()
	w.seedFor(w.Merchant.ID, c, kind, source, daysAgo, evidence)
}

func (w *world) seedFor(merchant string, c entity.Customer, kind, source string, daysAgo int, evidence string) {
	w.T.Helper()

	require.NoError(w.T, w.Store.RecordTrustEvent(w.Ctx, entity.TrustEvent{
		CustomerID: c.ID, Type: kind, Source: source, MerchantID: &merchant,
		Evidence: []byte(evidence), CreatedAt: time.Now().AddDate(0, 0, -daysAgo),
	}))
}

func (w *world) withTrust(labels bool) {
	w.UC.SetTrust(trust.New(w.Store, testutil.SilentLogger{}, trust.WithVendorLabels(labels)))
}

func TestTheVendorIsToldALabelAndFactsNotAScore(t *testing.T) {
	t.Run("a customer with no history is new", func(t *testing.T) {
		w := setup(t)
		w.withTrust(true)
		w.place(w.Rina, w.request())

		got := w.POS.Submitted[0].CustomerTrust
		require.NotNil(t, got)
		require.Equal(t, "NEW", got.Label)
		require.Zero(t, got.CompletedOrders)
		require.Zero(t, got.NoShowsLast90Days)
	})

	t.Run("a reliable customer", func(t *testing.T) {
		w := setup(t)
		w.withTrust(true)

		for i := 0; i < 5; i++ {
			w.seed(w.Rina, entity.EventOrderCompleted, entity.SourceSystem, 3+i, `{}`)
		}

		w.place(w.Rina, w.request())

		got := w.POS.Submitted[0].CustomerTrust
		require.Equal(t, "RELIABLE", got.Label)
		require.Equal(t, 5, got.CompletedOrders)
	})

	t.Run("a pattern of no-shows asks for caution, and then for prepayment", func(t *testing.T) {
		w := setup(t)
		w.withTrust(true)

		for i := 0; i < 5; i++ {
			w.seed(w.Rina, entity.EventOrderCompleted, entity.SourceSystem, 20+i, `{}`)
		}

		for i := 0; i < 3; i++ {
			w.seed(w.Rina, entity.EventNoShow, entity.SourceVendor, 1+i, `{"corroborated": true}`)
		}

		w.place(w.Rina, w.request())
		got := w.POS.Submitted[0].CustomerTrust
		require.Equal(t, "CAUTION", got.Label)
		require.Equal(t, 3, got.NoShowsLast90Days)

		next := func() *pos.CustomerTrust {
			t.Helper()

			_, err := w.UC.Cancel(w.Ctx, w.Rina.ID, firstOpen(t, w))
			_ = err
			w.place(w.Rina, w.request())

			return w.POS.Submitted[len(w.POS.Submitted)-1].CustomerTrust
		}

		// Three more from the same vendor change nothing: one vendor can only add so much.
		for i := 0; i < 3; i++ {
			w.seed(w.Rina, entity.EventNoShow, entity.SourceVendor, 5+i, `{"corroborated": true}`)
		}

		require.Equal(t, "CAUTION", next().Label)

		// It takes another vendor saying the same to move her further.
		other := w.AddMerchant("Warung Lain", "tenant-9", "sfk_aaaaaaaaaaaaaaaaaaaaaaaa", "sfs_lain")
		for i := 0; i < 3; i++ {
			w.seedFor(other.ID, w.Rina, entity.EventNoShow, entity.SourceVendor, 9+i, `{"corroborated": true}`)
		}

		last := next()
		require.Equal(t, "PREPAY_REQUIRED", last.Label)
		require.Equal(t, 9, last.NoShowsLast90Days)
	})

	t.Run("nothing is sent when labels are off", func(t *testing.T) {
		w := setup(t)
		w.withTrust(false)
		w.place(w.Rina, w.request())

		require.Nil(t, w.POS.Submitted[0].CustomerTrust)
	})

	t.Run("nothing is sent when the platform does not score at all", func(t *testing.T) {
		w := setup(t)
		w.place(w.Rina, w.request())

		require.Nil(t, w.POS.Submitted[0].CustomerTrust)
	})
}

func TestAVendorsRefusalOfACustomerIsNeverHeldAgainstThem(t *testing.T) {
	for _, code := range []string{"PREPAY_REQUIRED", "CUSTOMER_NOT_ACCEPTED"} {
		t.Run(code, func(t *testing.T) {
			w := setup(t)
			w.withTrust(true)
			w.POS.SubmitErr = &pos.Error{Status: 409, Code: code}

			o, _, err := w.UC.Checkout(w.Ctx, w.Rina, key(), w.request())

			var rejected *ordering.RejectedError

			require.ErrorAs(t, err, &rejected)
			require.Equal(t, code, rejected.Code)
			require.Equal(t, entity.OrderSubmitFailed, o.Status)
			require.Empty(t, w.trustEvents(w.Rina.ID), "being refused by a vendor is not something the customer did")
		})
	}
}

func rate(t *testing.T, w *world, o *entity.Order, rating string) {
	t.Helper()
	require.NoError(t, w.UC.ApplyRating(w.Ctx, w.Merchant.ID, posEvent(o.PlatformOrderID, "", map[string]any{"rating": rating})))
}

// finished makes an order that ended as the POS says, so it can be rated.
func (w *world) finished(c entity.Customer, status string, extra map[string]any) *entity.Order {
	w.T.Helper()

	o := w.place(c, w.request())
	require.NoError(w.T, w.UC.ApplyEvent(w.Ctx, 2, posEvent(o.PlatformOrderID, "ACCEPTED", nil)))
	require.NoError(w.T, w.UC.ApplyEvent(w.Ctx, 3, posEvent(o.PlatformOrderID, status, extra)))

	return o
}

func countOf(w *world, customerID, kind string) int {
	n := 0

	for _, ev := range w.trustEvents(customerID) {
		if ev.Type == kind {
			n++
		}
	}

	return n
}

func TestAVendorsThumbsAreRecordedOnceAndOnlyWhereTheyMakeSense(t *testing.T) {
	t.Run("a thumbs up on a completed order", func(t *testing.T) {
		w := setup(t)
		o := w.finished(w.Rina, "COMPLETED", nil)

		rate(t, w, o, "UP")
		rate(t, w, o, "UP")

		require.Equal(t, 1, countOf(w, w.Rina.ID, entity.EventVendorThumbsUp), "tapping twice counts once")
	})

	t.Run("a thumbs down with nothing else behind it is recorded as the vendor's word only", func(t *testing.T) {
		w := setup(t)
		o := w.finished(w.Rina, "COMPLETED", nil)

		rate(t, w, o, "DOWN")

		var found trustRow

		for _, ev := range w.trustEvents(w.Rina.ID) {
			if ev.Type == entity.EventVendorThumbsDown {
				found = ev
			}
		}

		require.Equal(t, entity.SourceVendor, found.Source)
		require.Equal(t, false, found.Evidence["corroborated"], "no fact of the platform's own backs it up")
	})

	t.Run("a change of mind does not count twice", func(t *testing.T) {
		w := setup(t)
		o := w.finished(w.Rina, "COMPLETED", nil)

		rate(t, w, o, "UP")
		rate(t, w, o, "DOWN")

		require.Equal(t, 1, countOf(w, w.Rina.ID, entity.EventVendorThumbsUp))
		require.Zero(t, countOf(w, w.Rina.ID, entity.EventVendorThumbsDown))
	})

	t.Run("a no-show is already a thumbs down: tapping adds nothing", func(t *testing.T) {
		w := setup(t)
		o := w.place(w.Rina, w.request())
		w.readyFor(o, 25*time.Minute)
		require.NoError(t, w.UC.ApplyEvent(w.Ctx, 5, posEvent(o.PlatformOrderID, "CANCELLED", map[string]any{"reasonCode": "NO_SHOW", "cancelledBy": "VENDOR"})))

		rate(t, w, o, "DOWN")

		require.Equal(t, 1, countOf(w, w.Rina.ID, entity.EventNoShow))
		require.Zero(t, countOf(w, w.Rina.ID, entity.EventVendorThumbsDown), "a no-show is one tap, not two")
	})

	t.Run("a thumbs up needs a completed order", func(t *testing.T) {
		w := setup(t)
		o := w.finished(w.Rina, "CANCELLED", map[string]any{"reasonCode": "OUT_OF_STOCK", "cancelledBy": "VENDOR"})

		rate(t, w, o, "UP")

		require.Zero(t, countOf(w, w.Rina.ID, entity.EventVendorThumbsUp))
	})

	t.Run("not while the order is still going on", func(t *testing.T) {
		w := setup(t)
		o := w.place(w.Rina, w.request())

		rate(t, w, o, "DOWN")

		require.Empty(t, w.trustEvents(w.Rina.ID))
	})

	t.Run("not after a week", func(t *testing.T) {
		w := setup(t)
		o := w.finished(w.Rina, "COMPLETED", nil)

		_, err := w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET updated_at = now() - interval '8 days' WHERE id = $1`, o.ID)
		require.NoError(t, err)

		rate(t, w, o, "UP")

		require.Zero(t, countOf(w, w.Rina.ID, entity.EventVendorThumbsUp))
	})

	t.Run("only the merchant that served the customer can rate them", func(t *testing.T) {
		w := setup(t)
		o := w.finished(w.Rina, "COMPLETED", nil)
		other := w.AddMerchant("Other", "tenant-2", "sfk_ffffffffffffffffffffffff", "sfs_other")

		require.NoError(t, w.UC.ApplyRating(w.Ctx, other.ID, posEvent(o.PlatformOrderID, "", map[string]any{"rating": "DOWN"})))

		require.Zero(t, countOf(w, w.Rina.ID, entity.EventVendorThumbsDown))
	})

	t.Run("nonsense is dropped without an error", func(t *testing.T) {
		w := setup(t)
		o := w.finished(w.Rina, "COMPLETED", nil)

		for _, bad := range []map[string]any{{"rating": "MEH"}, {"rating": ""}, {}} {
			require.NoError(t, w.UC.ApplyRating(w.Ctx, w.Merchant.ID, posEvent(o.PlatformOrderID, "", bad)))
		}

		require.NoError(t, w.UC.ApplyRating(w.Ctx, w.Merchant.ID, []byte(`not json`)))
		require.NoError(t, w.UC.ApplyRating(w.Ctx, w.Merchant.ID, posEvent("mo_unknown", "", map[string]any{"rating": "UP"})))
		require.Zero(t, countOf(w, w.Rina.ID, entity.EventVendorThumbsUp)+countOf(w, w.Rina.ID, entity.EventVendorThumbsDown))
	})
}

// firstOpen is the id of the customer Rina's one open order.
func firstOpen(t *testing.T, w *world) string {
	t.Helper()

	var id string
	require.NoError(t, w.Pool.QueryRow(w.Ctx,
		`SELECT id FROM merchant_orders WHERE customer_id = $1 AND status IN ('CREATED','PENDING_ACCEPTANCE','ACCEPTED','PREPARING','READY') LIMIT 1`,
		w.Rina.ID).Scan(&id))

	return id
}
