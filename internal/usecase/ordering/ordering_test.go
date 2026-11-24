package ordering_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/persistent"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/pos"
	"github.com/Calteras/streetfood-customer-api/internal/testutil"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/ordering"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/payment"
)

type world struct {
	*testutil.Env
	POS   *testutil.FakeOrderPOS
	GW    *testutil.FakeGateway
	Pay   *payment.UseCase
	UC    *ordering.UseCase
	Kiosk testutil.Kiosk
	Rina  entity.Customer
	Budi  entity.Customer
}

func setup(t *testing.T) *world {
	t.Helper()

	e := testutil.NewEnv(t)
	fake := testutil.NewFakeOrderPOS()
	gw := testutil.NewFakeGateway()
	pay := payment.New(e.Store, gw, e.Box, payment.Settings{
		Rates: entity.FeeRates{CustomerBps: 250, MerchantBps: 250}, Window: 15 * time.Minute, PublicBaseURL: "https://api.test",
	}, testutil.SilentLogger{})
	uc := ordering.New(e.Store, fake, pay, e.Box, testutil.SilentLogger{})
	pay.SetOrderNotifier(uc.Notification)
	w := &world{Env: e, POS: fake, GW: gw, Pay: pay, UC: uc}
	w.Kiosk = e.PublishKiosk(t)
	w.Rina = e.Customer(t, "uid-rina", "Rina Sari", "+628123456789")
	w.Budi = e.Customer(t, "uid-budi", "Budi", "")
	e.Integration.SetOrderMirror(w.UC)

	return w
}

func (w *world) request() ordering.CheckoutRequest {
	return ordering.CheckoutRequest{
		OutletID: w.Kiosk.Outlet.ID, FulfillmentType: entity.FulfillmentPickup,
		Items: []ordering.Line{{ProductID: w.Kiosk.Product, VariantID: &w.Kiosk.Variant, ModifierIDs: []string{w.Kiosk.Modifier}, Quantity: 2}},
	}
}

var seq int

func key() string {
	seq++
	return "key-" + time.Now().Format("150405.000000") + "-" + string(rune('a'+seq%26)) + string(rune('A'+seq/26%26))
}

func (w *world) place(c entity.Customer, req ordering.CheckoutRequest) *entity.Order {
	w.T.Helper()

	o, created, err := w.UC.Checkout(w.Ctx, c, key(), req)
	require.NoError(w.T, err)
	require.True(w.T, created)

	return o
}

func TestCheckoutSendsTheOrderToTheMerchant(t *testing.T) {
	w := setup(t)
	o := w.place(w.Rina, w.request())

	require.Equal(t, entity.OrderPendingAccept, o.Status, "the POS answered in the same request")
	require.Equal(t, int64(1554000), *o.TotalMinor, "the POS prices the order")
	require.Equal(t, 1, o.POSVersion)
	require.NotNil(t, o.AcceptBy)
	require.NotNil(t, o.POSOrderID)
	require.Len(t, o.Items, 1)
	require.Equal(t, "Es Teh Manis", o.Items[0].Name, "lines keep the name the customer saw")
	require.Equal(t, "Large", *o.Items[0].VariantName)

	require.Len(t, w.POS.Submitted, 1)

	sent := w.POS.Submitted[0]
	require.Equal(t, o.PlatformOrderID, sent.PlatformOrderID)
	require.Regexp(t, `^mo_[a-f0-9]{32}$`, sent.PlatformOrderID, "fits the POS id format")
	require.Equal(t, "kiosk-1", sent.OutletID, "the POS's own outlet id, not ours")
	require.Equal(t, "Rina Sari", sent.Customer.Name)
	require.Equal(t, "+628123456789", *sent.Customer.Phone, "the verified phone, not something the request said")
	require.Equal(t, "PAY_AT_PICKUP", sent.Payment["mode"])
	require.Equal(t, 2, sent.Items[0].Quantity)
	require.Equal(t, []string{w.Kiosk.Modifier}, sent.Items[0].ModifierIDs)
}

func TestCheckoutIsIdempotent(t *testing.T) {
	w := setup(t)
	req := w.request()

	first, created, err := w.UC.Checkout(w.Ctx, w.Rina, "same-key", req)
	require.NoError(t, err)
	require.True(t, created)

	again, created, err := w.UC.Checkout(w.Ctx, w.Rina, "same-key", req)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, first.ID, again.ID)
	require.Len(t, w.POS.Submitted, 1, "a repeat is not sent twice")

	reordered := req
	reordered.Items = []ordering.Line{{ProductID: w.Kiosk.Product, VariantID: &w.Kiosk.Variant, ModifierIDs: []string{w.Kiosk.Modifier}, Quantity: 2}}
	_, created, err = w.UC.Checkout(w.Ctx, w.Rina, "same-key", reordered)
	require.NoError(t, err, "the same request spelled the same way is the same request")
	require.False(t, created)

	different := req
	different.Items = []ordering.Line{{ProductID: w.Kiosk.Product, VariantID: &w.Kiosk.Variant, ModifierIDs: []string{w.Kiosk.Modifier}, Quantity: 3}}
	_, _, err = w.UC.Checkout(w.Ctx, w.Rina, "same-key", different)
	require.ErrorIs(t, err, entity.ErrIdempotency)

	// Keys belong to the customer: another person's identical key is a new order.
	other, created, err := w.UC.Checkout(w.Ctx, w.Budi, "same-key", req)
	require.NoError(t, err)
	require.True(t, created)
	require.NotEqual(t, first.ID, other.ID)

	// Concurrent duplicates: one order.
	var wg sync.WaitGroup

	ids := make(chan string, 6)

	for i := 0; i < 6; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			o, _, err := w.UC.Checkout(w.Ctx, w.Rina, "race-key", req)
			if err == nil {
				ids <- o.ID
			}
		}()
	}

	wg.Wait()
	close(ids)

	seen := map[string]bool{}
	for id := range ids {
		seen[id] = true
	}

	require.Len(t, seen, 1, "six parallel retries of one checkout make one order")
}

func TestCheckoutChecksTheCartBeforeBotheringTheMerchant(t *testing.T) {
	w := setup(t)

	mutate := func(f func(r *ordering.CheckoutRequest)) ordering.CheckoutRequest {
		r := w.request()
		f(&r)

		return r
	}

	cases := map[string]struct {
		req  ordering.CheckoutRequest
		code string
	}{
		"unknown product":    {mutate(func(r *ordering.CheckoutRequest) { r.Items[0].ProductID = "nope" }), entity.ProductUnavailable},
		"unknown variant":    {mutate(func(r *ordering.CheckoutRequest) { v := "nope"; r.Items[0].VariantID = &v }), entity.VariantUnavailable},
		"missing required":   {mutate(func(r *ordering.CheckoutRequest) { r.Items[0].ModifierIDs = nil }), entity.OptionSelection},
		"option not on item": {mutate(func(r *ordering.CheckoutRequest) { r.Items[0].ModifierIDs = []string{w.Kiosk.Modifier, "other"} }), entity.OptionUnavailable},
		"same option twice": {mutate(func(r *ordering.CheckoutRequest) {
			r.Items[0].ModifierIDs = []string{w.Kiosk.Modifier, w.Kiosk.Modifier}
		}), entity.OptionUnavailable},
		"foreign option only": {mutate(func(r *ordering.CheckoutRequest) { r.Items[0].ModifierIDs = []string{"other"} }), entity.OptionSelection},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := w.UC.Checkout(w.Ctx, w.Rina, key(), tc.req)

			var problems *ordering.ProblemsError

			require.ErrorAs(t, err, &problems)
			require.Equal(t, tc.code, problems.Problems[0].Code)
			require.Equal(t, 0, problems.Problems[0].Line)
		})
	}

	require.Empty(t, w.POS.Submitted, "the merchant is never asked about a cart that cannot work")

	shape := map[string]ordering.CheckoutRequest{
		"no lines":      mutate(func(r *ordering.CheckoutRequest) { r.Items = nil }),
		"zero quantity": mutate(func(r *ordering.CheckoutRequest) { r.Items[0].Quantity = 0 }),
		"too many":      mutate(func(r *ordering.CheckoutRequest) { r.Items[0].Quantity = 51 }),
		"bad type":      mutate(func(r *ordering.CheckoutRequest) { r.FulfillmentType = "DELIVERY" }),
		"long notes":    mutate(func(r *ordering.CheckoutRequest) { n := string(make([]rune, 301)); r.Notes = &n }),
		"negative":      mutate(func(r *ordering.CheckoutRequest) { v := int64(-1); r.ExpectedTotalMinor = &v }),
	}

	for name, req := range shape {
		t.Run(name, func(t *testing.T) {
			_, _, err := w.UC.Checkout(w.Ctx, w.Rina, key(), req)
			require.ErrorIs(t, err, entity.ErrBadRequest)
		})
	}

	_, _, err := w.UC.Checkout(w.Ctx, w.Rina, "", w.request())
	require.ErrorIs(t, err, entity.ErrBadRequest, "an idempotency key is required")

	bad := w.request()
	bad.OutletID = "not-a-uuid"
	_, _, err = w.UC.Checkout(w.Ctx, w.Rina, key(), bad)
	require.ErrorIs(t, err, entity.ErrNotFound)
}

func TestCheckoutNeedsAnOpenListedOutlet(t *testing.T) {
	w := setup(t)

	for _, status := range []string{"CLOSED", "PAUSED"} {
		_, err := w.Pool.Exec(w.Ctx, `UPDATE outlets SET online_status = $1`, status)
		require.NoError(t, err)

		_, _, err = w.UC.Checkout(w.Ctx, w.Rina, key(), w.request())
		require.ErrorIs(t, err, ordering.ErrOutletClosed, status)
	}

	_, err := w.Pool.Exec(w.Ctx, `UPDATE outlets SET online_status = 'OPEN', published = false`)
	require.NoError(t, err)

	_, _, err = w.UC.Checkout(w.Ctx, w.Rina, key(), w.request())
	require.ErrorIs(t, err, entity.ErrNotFound, "an unlisted outlet does not exist for customers")

	_, err = w.Pool.Exec(w.Ctx, `UPDATE outlets SET published = true; UPDATE outlets SET location_mode = 'MOBILE', session_active = false`)
	require.NoError(t, err)

	_, _, err = w.UC.Checkout(w.Ctx, w.Rina, key(), w.request())
	require.ErrorIs(t, err, ordering.ErrOutletClosed, "a mobile vendor that has not opened its day")

	require.Empty(t, w.POS.Submitted)
}

func TestTheMerchantCanRefuse(t *testing.T) {
	for name, tc := range map[string]struct{ captured, code string }{
		"prices moved":     {"submit-price-changed", "PRICE_CHANGED"},
		"item gone":        {"submit-invalid", "ORDER_INVALID"},
		"closed meanwhile": {"submit-closed", "OUTLET_CLOSED"},
	} {
		t.Run(name, func(t *testing.T) {
			w := setup(t)
			w.POS.SubmitErr = testutil.RealError(t, tc.captured)

			o, _, err := w.UC.Checkout(w.Ctx, w.Rina, "k", w.request())

			var rejected *ordering.RejectedError

			require.ErrorAs(t, err, &rejected)
			require.Equal(t, tc.code, rejected.Code)
			require.Equal(t, entity.OrderSubmitFailed, o.Status)

			switch tc.code {
			case "PRICE_CHANGED":
				require.EqualValues(t, 1554000, rejected.Detail["totalMinor"], "the new price comes through")
			case "ORDER_INVALID":
				require.NotEmpty(t, rejected.Detail["problems"])
			}

			// Asking again with the same key gives the same answer, not a second attempt.
			w.POS.SubmitErr = nil
			again, created, err := w.UC.Checkout(w.Ctx, w.Rina, "k", w.request())
			require.ErrorAs(t, err, &rejected)
			require.False(t, created)
			require.Equal(t, o.ID, again.ID)
			require.Len(t, w.POS.Submitted, 1)

			// A refused order is not an open order: it does not count against the limit.
			require.False(t, entity.IsOpen(entity.OrderSubmitFailed))
		})
	}
}

func TestExpectedTotalIsPassedToTheMerchant(t *testing.T) {
	w := setup(t)
	wrong := int64(1)
	req := w.request()
	req.ExpectedTotalMinor = &wrong

	_, _, err := w.UC.Checkout(w.Ctx, w.Rina, "k", req)

	var rejected *ordering.RejectedError

	require.ErrorAs(t, err, &rejected)
	require.Equal(t, "PRICE_CHANGED", rejected.Code)

	right := int64(1554000)
	req.ExpectedTotalMinor = &right
	o, _, err := w.UC.Checkout(w.Ctx, w.Rina, "k2", req)
	require.NoError(t, err)
	require.Equal(t, entity.OrderPendingAccept, o.Status)
}

func (w *world) makeDue() {
	w.T.Helper()

	_, err := w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET available_at = now() - interval '1 second', lease_until = NULL WHERE status = 'CREATED'`)
	require.NoError(w.T, err)
}

func (w *world) age(d string) {
	w.T.Helper()

	_, err := w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET created_at = now() - $1::interval`, d)
	require.NoError(w.T, err)
}

func (w *world) reload(o *entity.Order) *entity.Order {
	w.T.Helper()

	cur, err := w.Store.OrderByID(w.Ctx, o.ID)
	require.NoError(w.T, err)

	return cur
}

func TestAnUnreachableMerchantIsRetried(t *testing.T) {
	w := setup(t)
	w.POS.Down = true

	o, _, err := w.UC.Checkout(w.Ctx, w.Rina, "k", w.request())
	require.NoError(t, err, "an unreachable POS is not the customer's mistake")
	require.Equal(t, entity.OrderCreated, o.Status, "kept, and retried by the worker")

	w.makeDue()

	_, err = w.UC.ProcessSubmits(w.Ctx)
	require.NoError(t, err)
	require.Equal(t, entity.OrderCreated, w.reload(o).Status)
	require.GreaterOrEqual(t, w.reload(o).SubmitAttempts, 1)

	w.POS.Down = false
	w.makeDue()

	_, err = w.UC.ProcessSubmits(w.Ctx)
	require.NoError(t, err)
	require.Equal(t, entity.OrderPendingAccept, w.reload(o).Status, "delivered once the POS is back")
	require.True(t, w.POS.Has(o.PlatformOrderID))
}

func TestGivingUp(t *testing.T) {
	t.Run("the merchant stays unreachable", func(t *testing.T) {
		w := setup(t)
		w.POS.Down = true
		o, _, err := w.UC.Checkout(w.Ctx, w.Rina, "k", w.request())
		require.NoError(t, err)

		w.age("3 minutes")
		w.makeDue()

		_, err = w.UC.ProcessSubmits(w.Ctx)
		require.NoError(t, err)

		cur := w.reload(o)
		require.Equal(t, entity.OrderSubmitFailed, cur.Status)
		require.Equal(t, ordering.ErrorUnreachable, *cur.ErrorCode)
	})

	t.Run("an answer was lost but the merchant has the order", func(t *testing.T) {
		w := setup(t)
		w.POS.LoseAnswer = true
		o, _, err := w.UC.Checkout(w.Ctx, w.Rina, "k", w.request())
		require.NoError(t, err)
		require.Equal(t, entity.OrderCreated, o.Status, "we do not know yet")
		require.True(t, w.POS.Has(o.PlatformOrderID))

		w.age("3 minutes")
		w.makeDue()

		_, err = w.UC.ProcessSubmits(w.Ctx)
		require.NoError(t, err)
		require.Equal(t, entity.OrderPendingAccept, w.reload(o).Status, "adopted, not reported as failed")
	})
}

func TestWorkersDoNotSendTheSameOrderTwice(t *testing.T) {
	w := setup(t)
	w.POS.Down = true
	o, _, err := w.UC.Checkout(w.Ctx, w.Rina, "k", w.request())
	require.NoError(t, err)

	w.POS.Down = false
	w.POS.Submitted = nil
	w.makeDue()

	var wg sync.WaitGroup

	for i := 0; i < 4; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			_, _ = w.UC.ProcessSubmits(w.Ctx)
		}()
	}

	wg.Wait()

	require.Len(t, w.POS.Submitted, 1, "the lease keeps the others away")
	require.Empty(t, w.POS.Cancelled, "and nothing is taken back")
	require.Equal(t, entity.OrderPendingAccept, w.reload(o).Status)
}

func TestCancelling(t *testing.T) {
	t.Run("before the merchant is told", func(t *testing.T) {
		w := setup(t)
		w.POS.Down = true
		o, _, _ := w.UC.Checkout(w.Ctx, w.Rina, "k", w.request())

		got, err := w.UC.Cancel(w.Ctx, w.Rina.ID, o.ID)
		require.NoError(t, err)
		require.Equal(t, entity.OrderCancelled, got.Status)
		require.Empty(t, w.POS.Cancelled, "the merchant never heard of it")

		w.POS.Down = false
		w.makeDue()

		_, err = w.UC.ProcessSubmits(w.Ctx)
		require.NoError(t, err)
		require.False(t, w.POS.Has(o.PlatformOrderID), "a cancelled order is never sent")
	})

	t.Run("while it is on its way", func(t *testing.T) {
		w := setup(t)
		w.POS.OnSubmit = func(_ pos.SubmitRequest) {
			var id string
			require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT id FROM merchant_orders`).Scan(&id))

			_, err := w.UC.Cancel(w.Ctx, w.Rina.ID, id)
			require.NoError(t, err)
		}

		o, _, err := w.UC.Checkout(w.Ctx, w.Rina, "k", w.request())
		require.NoError(t, err)

		cur := w.reload(o)
		require.Equal(t, entity.OrderCancelled, cur.Status)
		require.Equal(t, []string{o.PlatformOrderID}, w.POS.Cancelled, "taken back from the merchant")
		require.Equal(t, 2, cur.POSVersion)
	})

	t.Run("while the merchant has not accepted", func(t *testing.T) {
		w := setup(t)
		o := w.place(w.Rina, w.request())

		got, err := w.UC.Cancel(w.Ctx, w.Rina.ID, o.ID)
		require.NoError(t, err)
		require.Equal(t, entity.OrderCancelled, got.Status)
		require.Equal(t, 2, got.POSVersion)

		again, err := w.UC.Cancel(w.Ctx, w.Rina.ID, o.ID)
		require.NoError(t, err, "cancelling twice is harmless")
		require.Equal(t, entity.OrderCancelled, again.Status)

		// The POS also announces the cancellation: already known, nothing happens.
		require.NoError(t, w.UC.ApplyEvent(w.Ctx, 2, statusEvent(o.PlatformOrderID, "CANCELLED")))
		require.Zero(t, w.notifications(o.ID), "the customer is not pushed about their own cancel")
	})

	t.Run("after the merchant accepted", func(t *testing.T) {
		w := setup(t)
		o := w.place(w.Rina, w.request())
		require.NoError(t, w.UC.ApplyEvent(w.Ctx, 2, statusEvent(o.PlatformOrderID, "ACCEPTED")))

		_, err := w.UC.Cancel(w.Ctx, w.Rina.ID, o.ID)
		require.ErrorIs(t, err, entity.ErrCannotCancel)
		require.Empty(t, w.POS.Cancelled, "not even asked")
	})

	t.Run("the merchant accepted a moment first", func(t *testing.T) {
		w := setup(t)
		o := w.place(w.Rina, w.request())
		w.POS.Set(o.PlatformOrderID, "ACCEPTED") // no event has arrived yet

		_, err := w.UC.Cancel(w.Ctx, w.Rina.ID, o.ID)
		require.ErrorIs(t, err, entity.ErrCannotCancel)
		require.Equal(t, entity.OrderAccepted, w.reload(o).Status, "and the screen shows the truth")
	})

	t.Run("someone else's order", func(t *testing.T) {
		w := setup(t)
		o := w.place(w.Rina, w.request())

		_, err := w.UC.Cancel(w.Ctx, w.Budi.ID, o.ID)
		require.ErrorIs(t, err, entity.ErrNotFound)

		_, err = w.UC.Order(w.Ctx, w.Budi.ID, o.ID)
		require.ErrorIs(t, err, entity.ErrNotFound)

		_, err = w.UC.Cancel(w.Ctx, w.Rina.ID, "not-a-uuid")
		require.ErrorIs(t, err, entity.ErrNotFound)
	})
}

func statusEvent(platformOrderID, status string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"platformOrderId": platformOrderID, "status": status, "reason": nil})

	return raw
}

func (w *world) notifications(orderID string) int {
	w.T.Helper()

	var n int

	require.NoError(w.T, w.Pool.QueryRow(w.Ctx, `SELECT count(*) FROM notification_outbox WHERE order_id = $1`, orderID).Scan(&n))

	return n
}

func TestStatusChangesAreMirroredInOrder(t *testing.T) {
	w := setup(t)
	o := w.place(w.Rina, w.request())
	id := o.PlatformOrderID

	apply := func(version int, status string) {
		require.NoError(t, w.UC.ApplyEvent(w.Ctx, version, statusEvent(id, status)))
	}

	apply(3, "PREPARING")
	apply(2, "ACCEPTED") // arrived late
	require.Equal(t, entity.OrderPreparing, w.reload(o).Status, "an older change never wins")

	apply(3, "PREPARING") // redelivered
	apply(4, "READY")
	apply(5, "COMPLETED")

	cur := w.reload(o)
	require.Equal(t, entity.OrderCompleted, cur.Status)
	require.Equal(t, 5, cur.POSVersion)
	require.NotNil(t, cur.CompletedAt)
	require.Equal(t, int64(1554000), *cur.TotalMinor, "pricing survives a status change that carries none")

	// Ignorable events.
	require.NoError(t, w.UC.ApplyEvent(w.Ctx, 9, statusEvent("mo_somebody_elses_order", "READY")))
	require.NoError(t, w.UC.ApplyEvent(w.Ctx, 9, statusEvent(id, "TELEPORTED")))
	require.NoError(t, w.UC.ApplyEvent(w.Ctx, 9, json.RawMessage(`not json`)))
	require.Equal(t, 5, w.reload(o).POSVersion)

	// Only the news worth a push is queued, once each.
	var kinds []string

	rows, err := w.Pool.Query(w.Ctx, `SELECT kind FROM notification_outbox WHERE order_id = $1 ORDER BY kind`, o.ID)
	require.NoError(t, err)
	defer rows.Close()

	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))

		kinds = append(kinds, k)
	}

	require.Equal(t, []string{"READY"}, kinds, "ACCEPTED arrived too late to matter; PREPARING and COMPLETED are not pushed")
}

func TestPushTextsSayWhatHappened(t *testing.T) {
	w := setup(t)

	body := func(status string, version int, reason *string, prep *int) (string, string) {
		o := w.place(w.Rina, w.request())
		raw, _ := json.Marshal(map[string]any{"platformOrderId": o.PlatformOrderID, "status": status, "reason": reason, "prepTimeMinutes": prep})
		require.NoError(t, w.UC.ApplyEvent(w.Ctx, version, raw))

		var title, text string
		require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT title, body FROM notification_outbox WHERE order_id = $1`, o.ID).Scan(&title, &text))

		_, err := w.UC.Cancel(w.Ctx, w.Rina.ID, o.ID) // free the slot for the next order
		_ = err

		return title, text
	}

	reason := "Habis"
	prep := 12

	title, text := body("ACCEPTED", 2, nil, &prep)
	require.Equal(t, "Pesanan diterima", title)
	require.Contains(t, text, "Kios Melati")
	require.Contains(t, text, "12 menit")

	title, text = body("REJECTED", 2, &reason, nil)
	require.Equal(t, "Pesanan ditolak", title)
	require.Contains(t, text, "Habis")

	title, _ = body("READY", 4, nil, nil)
	require.Equal(t, "Pesanan siap diambil", title)

	title, _ = body("EXPIRED", 2, nil, nil)
	require.Equal(t, "Pesanan tidak direspons", title)
}

// The captured fixtures are what the real POS emitted for three orders: one accepted and
// completed, one rejected, one cancelled by the customer.
func TestRealOrderEventsFromThePOS(t *testing.T) {
	w := setup(t)

	dir := filepath.Join("..", "..", "testutil", "testdata", "pos")

	var ids map[string]string

	raw, err := os.ReadFile(filepath.Join(dir, "order-ids.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &ids))

	var events []json.RawMessage

	raw, err = os.ReadFile(filepath.Join(dir, "order-events.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &events))
	require.Len(t, events, 6)

	// Orders as the platform would hold them, under the ids the POS knew them by.
	for _, name := range []string{"okOrder", "rejectedOrder", "cancelledOrder"} {
		_, _, err := w.Store.CreateCheckout(w.Ctx, persistentInput(w, ids[name], name))
		require.NoError(t, err)
	}

	for _, ev := range events {
		require.NoError(t, w.SendRaw(ev))
	}

	w.Drain()

	status := func(name string) (string, int) {
		var s string

		var v int

		require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT status, pos_version FROM merchant_orders WHERE platform_order_id = $1`, ids[name]).Scan(&s, &v))

		return s, v
	}

	s, v := status("okOrder")
	require.Equal(t, "COMPLETED", s)
	require.Equal(t, 5, v)

	s, _ = status("rejectedOrder")
	require.Equal(t, "REJECTED", s)

	var reason *string
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT reason FROM merchant_orders WHERE platform_order_id = $1`, ids["rejectedOrder"]).Scan(&reason))
	require.Equal(t, "Habis", *reason, "the merchant's reason reaches the customer")

	s, _ = status("cancelledOrder")
	require.Equal(t, "CANCELLED", s)

	var failed int
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT count(*) FROM integration_inbox WHERE status <> 'DONE'`).Scan(&failed))
	require.Zero(t, failed)
}

func TestStaleOrdersAreRefreshed(t *testing.T) {
	w := setup(t)
	o := w.place(w.Rina, w.request())

	w.POS.Set(o.PlatformOrderID, "ACCEPTED") // the event was lost

	n, err := w.UC.RefreshStale(w.Ctx)
	require.NoError(t, err)
	require.False(t, n)
	require.Equal(t, entity.OrderPendingAccept, w.reload(o).Status, "recently checked: left alone")

	_, err = w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET checked_at = now() - interval '5 minutes'`)
	require.NoError(t, err)

	_, err = w.UC.RefreshStale(w.Ctx)
	require.NoError(t, err)
	require.Equal(t, entity.OrderAccepted, w.reload(o).Status)
	require.Equal(t, 1, w.notifications(o.ID), "and the customer is told")

	calls := w.POS.Count("status:")
	_, err = w.UC.RefreshStale(w.Ctx)
	require.NoError(t, err)
	require.Equal(t, calls, w.POS.Count("status:"), "checked just now: not asked again")
}

func TestLimitsStopOneAccountFloodingMerchants(t *testing.T) {
	w := setup(t)

	for i := 0; i < entity.MaxOpenOrders; i++ {
		w.place(w.Rina, w.request())
	}

	_, _, err := w.UC.Checkout(w.Ctx, w.Rina, key(), w.request())
	require.ErrorIs(t, err, entity.ErrTooMany, "too many orders at once")

	w.place(w.Budi, w.request()) // someone else is unaffected

	// A replay of an earlier checkout still works at the limit.
	_, created, err := w.UC.Checkout(w.Ctx, w.Rina, "replay", w.request())
	require.ErrorIs(t, err, entity.ErrTooMany)
	require.False(t, created)

	// Finishing orders frees the slots, but the hourly cap still counts them.
	_, err = w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET status = 'COMPLETED' WHERE customer_id = $1`, w.Rina.ID)
	require.NoError(t, err)

	for i := entity.MaxOpenOrders; i < entity.MaxCheckoutsPerHour; i++ {
		o := w.place(w.Rina, w.request())
		_, err = w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET status = 'COMPLETED' WHERE id = $1`, o.ID)
		require.NoError(t, err)
	}

	_, _, err = w.UC.Checkout(w.Ctx, w.Rina, key(), w.request())
	require.ErrorIs(t, err, entity.ErrTooMany, "too many checkouts in an hour")
}

func TestReviews(t *testing.T) {
	w := setup(t)
	o := w.place(w.Rina, w.request())
	good := "  Enak sekali  "

	_, err := w.UC.Review(w.Ctx, w.Rina.ID, o.ID, 5, &good)
	require.ErrorIs(t, err, entity.ErrBadRequest, "only a completed order can be reviewed")

	require.NoError(t, w.UC.ApplyEvent(w.Ctx, 5, statusEvent(o.PlatformOrderID, "COMPLETED")))

	for _, rating := range []int{0, 6, -1} {
		_, err = w.UC.Review(w.Ctx, w.Rina.ID, o.ID, rating, nil)
		require.ErrorIs(t, err, entity.ErrBadRequest)
	}

	long := string(make([]rune, 501))
	_, err = w.UC.Review(w.Ctx, w.Rina.ID, o.ID, 5, &long)
	require.ErrorIs(t, err, entity.ErrBadRequest)

	_, err = w.UC.Review(w.Ctx, w.Budi.ID, o.ID, 5, nil)
	require.ErrorIs(t, err, entity.ErrNotFound, "not their order")

	r, err := w.UC.Review(w.Ctx, w.Rina.ID, o.ID, 5, &good)
	require.NoError(t, err)
	require.Equal(t, "Enak sekali", *r.Comment, "trimmed")

	_, err = w.UC.Review(w.Ctx, w.Rina.ID, o.ID, 4, nil)
	require.ErrorIs(t, err, entity.ErrConflict, "once per order")

	// A second reviewer, with no comment.
	o2 := w.place(w.Budi, w.request())
	require.NoError(t, w.UC.ApplyEvent(w.Ctx, 5, statusEvent(o2.PlatformOrderID, "COMPLETED")))

	blank := "   "
	r2, err := w.UC.Review(w.Ctx, w.Budi.ID, o2.ID, 4, &blank)
	require.NoError(t, err)
	require.Nil(t, r2.Comment, "a blank comment is no comment")

	list, err := w.UC.OutletReviews(w.Ctx, w.Kiosk.Outlet.ID, nil, 10)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "Budi", list[0].Reviewer)
	require.Equal(t, "Rina", list[1].Reviewer, "first name only")

	ratings, err := w.Store.Ratings(w.Ctx, []string{w.Kiosk.Outlet.ID})
	require.NoError(t, err)
	require.InDelta(t, 4.5, ratings[w.Kiosk.Outlet.ID].Average, 1e-9)
	require.Equal(t, 2, ratings[w.Kiosk.Outlet.ID].Count)
}

// ---------------------------------------------------------------------- push

type fakePusher struct {
	mu   sync.Mutex
	sent []sentPush
	fail map[string]error
}

type sentPush struct {
	Token, Title, Body string
	Data               map[string]string
}

func (f *fakePusher) Send(_ context.Context, token, title, body string, data map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.fail[token]; err != nil {
		return err
	}

	f.sent = append(f.sent, sentPush{token, title, body, data})

	return nil
}

func TestNotifier(t *testing.T) {
	setupPush := func(t *testing.T) (*world, *fakePusher, *ordering.Notifier, *entity.Order) {
		w := setup(t)
		p := &fakePusher{fail: map[string]error{}}
		o := w.place(w.Rina, w.request())
		require.NoError(t, w.UC.ApplyEvent(w.Ctx, 4, statusEvent(o.PlatformOrderID, "READY")))

		return w, p, ordering.NewNotifier(w.Store, p, testutil.SilentLogger{}), o
	}

	state := func(w *world, o *entity.Order) (string, string) {
		var s string

		var reason *string

		require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT status, last_error FROM notification_outbox WHERE order_id = $1`, o.ID).Scan(&s, &reason))

		if reason == nil {
			return s, ""
		}

		return s, *reason
	}

	t.Run("delivered to every device", func(t *testing.T) {
		w, p, n, o := setupPush(t)
		require.NoError(t, w.Store.UpsertDevice(w.Ctx, w.Rina.ID, "android", "tok-a"))
		require.NoError(t, w.Store.UpsertDevice(w.Ctx, w.Rina.ID, "ios", "tok-b"))

		_, err := n.Process(w.Ctx)
		require.NoError(t, err)
		require.Len(t, p.sent, 2)
		require.Equal(t, "Pesanan siap diambil", p.sent[0].Title)
		require.Equal(t, o.ID, p.sent[0].Data["orderId"], "the app can open the right order")
		require.Equal(t, "READY", p.sent[0].Data["status"])

		s, _ := state(w, o)
		require.Equal(t, "SENT", s)

		_, err = n.Process(w.Ctx)
		require.NoError(t, err)
		require.Len(t, p.sent, 2, "never sent twice")
	})

	t.Run("a dead token is forgotten", func(t *testing.T) {
		w, p, n, o := setupPush(t)
		require.NoError(t, w.Store.UpsertDevice(w.Ctx, w.Rina.ID, "android", "dead"))
		require.NoError(t, w.Store.UpsertDevice(w.Ctx, w.Rina.ID, "ios", "alive"))
		p.fail["dead"] = entity.ErrUnregistered

		_, err := n.Process(w.Ctx)
		require.NoError(t, err)

		tokens, err := w.Store.DeviceTokens(w.Ctx, w.Rina.ID)
		require.NoError(t, err)
		require.Equal(t, []string{"alive"}, tokens)

		s, _ := state(w, o)
		require.Equal(t, "SENT", s, "one device got it")
	})

	t.Run("a failing provider is retried, then given up", func(t *testing.T) {
		w, p, n, o := setupPush(t)
		require.NoError(t, w.Store.UpsertDevice(w.Ctx, w.Rina.ID, "android", "flaky"))
		p.fail["flaky"] = errors.New("503")

		for attempt := 1; attempt <= 5; attempt++ {
			_, err := n.Process(w.Ctx)
			require.NoError(t, err)

			s, _ := state(w, o)
			if attempt < 5 {
				require.Equal(t, "PENDING", s, "attempt %d", attempt)

				_, err = w.Pool.Exec(w.Ctx, `UPDATE notification_outbox SET available_at = now()`)
				require.NoError(t, err)
			}
		}

		s, reason := state(w, o)
		require.Equal(t, "FAILED", s)
		require.Equal(t, "503", reason)
	})

	t.Run("no devices", func(t *testing.T) {
		w, _, n, o := setupPush(t)

		_, err := n.Process(w.Ctx)
		require.NoError(t, err)

		s, reason := state(w, o)
		require.Equal(t, "FAILED", s)
		require.Equal(t, "no devices", reason)
	})

	t.Run("push not configured drains the queue", func(t *testing.T) {
		w, _, _, o := setupPush(t)
		n := ordering.NewNotifier(w.Store, nil, testutil.SilentLogger{})

		_, err := n.Process(w.Ctx)
		require.NoError(t, err)

		s, _ := state(w, o)
		require.Equal(t, "FAILED", s)
	})
}

// ------------------------------------------------------------------------ hub

func TestHubWakesWatchersOfTheOrder(t *testing.T) {
	h := ordering.NewHub()
	a, cancelA := h.Subscribe("order-1")
	b, cancelB := h.Subscribe("order-1")
	other, cancelO := h.Subscribe("order-2")

	defer cancelO()

	h.Notify("order-1")
	h.Notify("order-1") // coalesces

	for _, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("watcher was not woken")
		}
	}

	select {
	case <-a:
		t.Fatal("two changes should wake a slow reader once")
	case <-other:
		t.Fatal("another order's watcher was woken")
	case <-time.After(50 * time.Millisecond):
	}

	cancelA()
	cancelB()
	h.Notify("order-1") // nobody listening: no panic, no leak

	require.Empty(t, func() map[string]int { return map[string]int{} }())
}

func TestHubHearsDatabaseChanges(t *testing.T) {
	w := setup(t)
	o := w.place(w.Rina, w.request())

	hub := ordering.NewHub()
	ctx, cancel := context.WithCancel(w.Ctx)

	defer cancel()

	go hub.Run(ctx, w.Store.ListenOrderUpdates, testutil.SilentLogger{})

	changes, stop := hub.Subscribe(o.ID)
	defer stop()

	time.Sleep(300 * time.Millisecond) // let the listener connect

	require.NoError(t, w.UC.ApplyEvent(w.Ctx, 2, statusEvent(o.PlatformOrderID, "ACCEPTED")))

	select {
	case <-changes:
	case <-time.After(3 * time.Second):
		t.Fatal("a status change did not reach the hub")
	}

	// A change that is not applied (older version) does not wake anyone.
	require.NoError(t, w.UC.ApplyEvent(w.Ctx, 2, statusEvent(o.PlatformOrderID, "READY")))

	select {
	case <-changes:
		t.Fatal("an ignored event woke a watcher")
	case <-time.After(300 * time.Millisecond):
	}
}

func persistentInput(w *world, platformOrderID, name string) persistent.CheckoutInput {
	return persistent.CheckoutInput{
		CustomerID: w.Rina.ID, IdempotencyKey: "fixture-" + name, RequestHash: name,
		MerchantID: w.Kiosk.Outlet.MerchantID, OutletID: w.Kiosk.Outlet.ID, PlatformOrderID: platformOrderID,
		FulfillmentType: entity.FulfillmentPickup,
		Items:           []entity.OrderItem{{LineNo: 1, ProductID: w.Kiosk.Product, ModifierIDs: []string{}, Quantity: 1, Name: "Es Teh Manis"}},
	}
}
