package payment_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/gateway"
	"github.com/Calteras/streetfood-customer-api/internal/testutil"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/ordering"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/payment"
)

type world struct {
	*testutil.Env
	POS   *testutil.FakeOrderPOS
	GW    *testutil.FakeGateway
	Pay   *payment.UseCase
	Order *ordering.UseCase
	Kiosk testutil.Kiosk
	Rina  entity.Customer
}

// The quoted total the fake POS answers with, and what the customer is charged on top of it
// at 2.5%: Rp 15.540 + Rp 389 = Rp 15.929.
const (
	quoted = int64(1554000)
	charge = int64(1592900)
)

func setup(t *testing.T) *world {
	t.Helper()

	e := testutil.NewEnv(t)
	fake := testutil.NewFakeOrderPOS()
	gw := testutil.NewFakeGateway()
	pay := payment.New(e.Store, gw, e.Box, payment.Settings{
		Rates: entity.FeeRates{CustomerBps: 250, MerchantBps: 250}, Window: 15 * time.Minute, PublicBaseURL: "https://api.test/",
	}, testutil.SilentLogger{})
	uc := ordering.New(e.Store, fake, pay, e.Box, testutil.SilentLogger{})
	pay.SetOrderNotifier(uc.Notification)
	e.Integration.SetOrderMirror(uc)

	w := &world{Env: e, POS: fake, GW: gw, Pay: pay, Order: uc}
	w.Kiosk = e.PublishKiosk(t)
	w.Rina = e.Customer(t, "uid-rina", "Rina Sari", "+628123456789")

	return w
}

// connect gives the merchant a gateway account.
func (w *world) connect() payment.GatewayView {
	w.T.Helper()

	v, err := w.Pay.SetGateway(w.Ctx, w.Merchant.ID, payment.GatewayInput{Provider: "xendit", Secret: "xnd_development_secret", CallbackToken: testutil.CallbackToken})
	require.NoError(w.T, err)

	return v
}

func (w *world) request() ordering.CheckoutRequest {
	return ordering.CheckoutRequest{
		OutletID: w.Kiosk.Outlet.ID, FulfillmentType: entity.FulfillmentPickup, Payment: entity.PaymentModeOnline,
		Items: []ordering.Line{{ProductID: w.Kiosk.Product, VariantID: &w.Kiosk.Variant, ModifierIDs: []string{w.Kiosk.Modifier}, Quantity: 2}},
	}
}

var seq int

func key() string {
	seq++

	return time.Now().Format("150405.000000") + "-" + string(rune('a'+seq%26))
}

// order places an online order and returns it unpaid, with its one open attempt.
func (w *world) order() *entity.Order {
	w.T.Helper()

	o, created, err := w.Order.Checkout(w.Ctx, w.Rina, key(), w.request())
	require.NoError(w.T, err)
	require.True(w.T, created)

	return o
}

// pay makes the customer pay the open attempt and the gateway notify us.
func (w *world) pay(o *entity.Order) *entity.Order {
	w.T.Helper()

	p := w.reload(o).Payment
	require.Equal(w.T, entity.PaymentPending, p.Status)
	w.GW.Settle(p.ExternalID, gateway.StatePaid, "QRIS")
	w.notify(p)

	return w.reload(o)
}

// notify delivers a gateway notification for an attempt, as the gateway does.
func (w *world) notify(p *entity.Payment) error {
	w.T.Helper()

	g, err := w.Store.GatewayByMerchant(w.Ctx, w.Merchant.ID)
	require.NoError(w.T, err)

	return w.Pay.Callback(w.Ctx, "xendit", g.ID,
		func(h string) string {
			if h == "X-Callback-Token" {
				return testutil.CallbackToken
			}

			return ""
		}, []byte(`{"external_id":"`+p.ExternalID+`"}`))
}

func (w *world) reload(o *entity.Order) *entity.Order {
	w.T.Helper()

	got, err := w.Store.OrderByID(w.Ctx, o.ID)
	require.NoError(w.T, err)

	return got
}

// send runs the worker that hands paid orders to the POS.
func (w *world) send() {
	w.T.Helper()

	_, err := w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET available_at = now() WHERE status = 'CREATED'`)
	require.NoError(w.T, err)

	_, err = w.Order.ProcessSubmits(w.Ctx)
	require.NoError(w.T, err)
}

func (w *world) pushes(o *entity.Order) []string {
	w.T.Helper()

	rows, err := w.Pool.Query(w.Ctx, `SELECT kind FROM notification_outbox WHERE order_id = $1 ORDER BY created_at, kind`, o.ID)
	require.NoError(w.T, err)
	defer rows.Close()

	var kinds []string

	for rows.Next() {
		var k string
		require.NoError(w.T, rows.Scan(&k))

		kinds = append(kinds, k)
	}

	return kinds
}

func TestAnOnlineOrderWaitsForPaymentBeforeTheMerchantSeesIt(t *testing.T) {
	w := setup(t)
	w.connect()

	o := w.order()

	require.Equal(t, entity.OrderUnpaid, o.Status)
	require.Equal(t, entity.PaymentModeOnline, o.PaymentMode)
	require.Equal(t, quoted, *o.TotalMinor, "priced by the POS before anyone is charged")
	require.NotNil(t, o.PaymentDeadline)
	require.WithinDuration(t, time.Now().Add(15*time.Minute), *o.PaymentDeadline, 10*time.Second)
	require.Empty(t, w.POS.Submitted, "the merchant knows nothing until it is paid")

	p := o.Payment
	require.NotNil(t, p)
	require.Equal(t, entity.PaymentPending, p.Status)
	require.Equal(t, 1, p.Attempt)
	require.Equal(t, charge, p.AmountMinor, "order total plus the 2.5% customer fee, in whole rupiah")
	require.EqualValues(t, 38900, p.CustomerFeeMinor)
	require.EqualValues(t, 38900, p.MerchantFeeMinor)
	require.NotNil(t, p.PayURL)

	_, c := w.GW.Only()
	require.EqualValues(t, 15929, c.Request.AmountRupiah, "the gateway takes whole rupiah, not minor units")
	require.Regexp(t, `^sfp_[a-f0-9]{32}$`, c.Request.ExternalID)
	require.WithinDuration(t, *o.PaymentDeadline, c.Request.ExpiresAt, time.Second, "the payment page expires with the order")
}

func TestPayingSendsTheOrderToTheMerchantAsPrepaid(t *testing.T) {
	w := setup(t)
	w.connect()

	o := w.pay(w.order())
	require.Equal(t, entity.OrderCreated, o.Status, "paid: now it goes to the merchant")
	require.Equal(t, entity.PaymentPaid, o.Payment.Status)
	require.NotNil(t, o.Payment.PaidAt)
	require.Equal(t, "QRIS", *o.Payment.Method)
	require.Equal(t, []string{"PAYMENT_PAID"}, w.pushes(o), "the customer is told the payment worked")

	w.send()

	o = w.reload(o)
	require.Equal(t, entity.OrderPendingAccept, o.Status)
	require.Len(t, w.POS.Submitted, 1)

	sent := w.POS.Submitted[0]
	require.Equal(t, "PREPAID", sent.Payment["mode"])
	require.Equal(t, "QRIS", sent.Payment["method"])
	require.Equal(t, o.Payment.ExternalID, sent.Payment["reference"])
	require.Equal(t, quoted, *sent.ExpectedTotalMinor, "the POS must charge what the customer paid for")
}

func TestARepeatedNotificationChangesNothing(t *testing.T) {
	w := setup(t)
	w.connect()

	o := w.pay(w.order())
	p := o.Payment

	for i := 0; i < 3; i++ {
		require.NoError(t, w.notify(p))
	}

	require.Equal(t, []string{"PAYMENT_PAID"}, w.pushes(o))
	require.Equal(t, entity.OrderCreated, w.reload(o).Status)
}

func TestACallbackProvesItself(t *testing.T) {
	w := setup(t)
	view := w.connect()
	o := w.order()
	p := o.Payment
	w.GW.Settle(p.ExternalID, gateway.StatePaid, "QRIS")

	body := []byte(`{"external_id":"` + p.ExternalID + `"}`)
	header := func(string) string { return "wrong-token" }

	require.ErrorIs(t, w.Pay.Callback(w.Ctx, "xendit", view.ID, header, body), entity.ErrUnauthorized)
	require.Equal(t, entity.OrderUnpaid, w.reload(o).Status, "a forged notification moves nothing, even if the gateway would say paid")

	right := func(string) string { return testutil.CallbackToken }
	require.ErrorIs(t, w.Pay.Callback(w.Ctx, "xendit", "00000000-0000-0000-0000-000000000000", right, body), entity.ErrNotFound)
	require.ErrorIs(t, w.Pay.Callback(w.Ctx, "xendit", "nonsense", right, body), entity.ErrNotFound)
	require.ErrorIs(t, w.Pay.Callback(w.Ctx, "midtrans", view.ID, right, body), entity.ErrNotFound, "the gateway's id belongs to its own provider")
	require.NoError(t, w.Pay.Callback(w.Ctx, "xendit", view.ID, right, []byte(`{"external_id":"sfp_unknown"}`)), "a test ping or another system's payment is not an error")
	require.Equal(t, entity.OrderUnpaid, w.reload(o).Status)
}

func TestTheGatewayDecidesNotTheNotification(t *testing.T) {
	w := setup(t)
	w.connect()
	o := w.order()

	// A notification that claims a payment the gateway does not know of changes nothing.
	require.NoError(t, w.notify(o.Payment))
	require.Equal(t, entity.OrderUnpaid, w.reload(o).Status)
	require.Equal(t, entity.PaymentPending, w.reload(o).Payment.Status)

	// A payment of less than the charge does not release the order.
	_, c := w.GW.Only()
	w.GW.Settle(o.Payment.ExternalID, gateway.StatePaid, "QRIS")
	c.Paid = 100

	require.NoError(t, w.notify(o.Payment))

	got := w.reload(o)
	require.Equal(t, entity.OrderUnpaid, got.Status)
	require.Equal(t, entity.PaymentFailed, got.Payment.Status)
	require.Equal(t, "AMOUNT_MISMATCH", *got.Payment.ErrorCode)
}

func TestAFailedPaymentLeavesTheOrderUnpaidAndTheCustomerCanRetry(t *testing.T) {
	w := setup(t)
	w.connect()
	o := w.order()
	first := o.Payment

	w.GW.Settle(first.ExternalID, gateway.StateFailed, "")
	require.NoError(t, w.notify(first))

	o = w.reload(o)
	require.Equal(t, entity.OrderUnpaid, o.Status, "still unpaid, still there")
	require.Equal(t, entity.PaymentFailed, o.Payment.Status)
	require.Equal(t, "PAYMENT_DECLINED", *o.Payment.ErrorCode)
	require.Equal(t, []string{"PAYMENT_FAILED_1"}, w.pushes(o), "the customer is told clearly")
	require.Empty(t, w.POS.Submitted)

	// Retrying opens a second attempt.
	retried, err := w.Pay.Retry(w.Ctx, w.Rina.ID, o.ID)
	require.NoError(t, err)
	require.Equal(t, 2, retried.Payment.Attempt)
	require.Equal(t, entity.PaymentPending, retried.Payment.Status)
	require.NotEqual(t, first.ExternalID, retried.Payment.ExternalID)
	require.Len(t, w.GW.Charges, 2)

	// Retrying again while that is open returns it instead of opening a third.
	again, err := w.Pay.Retry(w.Ctx, w.Rina.ID, o.ID)
	require.NoError(t, err)
	require.Equal(t, retried.Payment.ID, again.Payment.ID)
	require.Len(t, w.GW.Charges, 2)

	// Paying the new attempt works, and a second failure would have its own message.
	w.GW.Settle(retried.Payment.ExternalID, gateway.StatePaid, "QRIS")
	require.NoError(t, w.notify(retried.Payment))
	require.Equal(t, entity.OrderCreated, w.reload(o).Status)

	_, err = w.Pay.Retry(w.Ctx, w.Rina.ID, o.ID)
	require.ErrorIs(t, err, entity.ErrNotPayable, "a paid order cannot be paid again")
}

func TestAnotherCustomerCannotPayYourOrder(t *testing.T) {
	w := setup(t)
	w.connect()
	o := w.order()

	budi := w.Customer(t, "uid-budi", "Budi", "")
	_, err := w.Pay.Retry(w.Ctx, budi.ID, o.ID)
	require.ErrorIs(t, err, entity.ErrNotFound)
}

func TestWhenTheGatewayIsDownTheOrderIsKeptAndPaymentCanBeRetried(t *testing.T) {
	w := setup(t)
	w.connect()
	w.GW.CreateErr = errors.New("connection refused")

	o := w.order()
	require.Equal(t, entity.OrderUnpaid, o.Status)
	require.Equal(t, entity.PaymentFailed, o.Payment.Status)
	require.Equal(t, "GATEWAY_UNAVAILABLE", *o.Payment.ErrorCode)
	require.Nil(t, o.Payment.PayURL)

	w.GW.CreateErr = nil

	retried, err := w.Pay.Retry(w.Ctx, w.Rina.ID, o.ID)
	require.NoError(t, err)
	require.Equal(t, entity.PaymentPending, retried.Payment.Status)
	require.NotNil(t, retried.Payment.PayURL)

	w.GW.CreateErr = &gateway.Error{Status: 400, Code: "INVALID", Message: "no"}
	w.GW.Settle(retried.Payment.ExternalID, gateway.StateFailed, "")
	require.NoError(t, w.notify(retried.Payment))

	again, err := w.Pay.Retry(w.Ctx, w.Rina.ID, o.ID)
	require.NoError(t, err)
	require.Equal(t, "GATEWAY_REJECTED", *again.Payment.ErrorCode, "a refusal is told apart from an outage")
}

func TestAnUnpaidOrderExpiresAfterItsDeadline(t *testing.T) {
	w := setup(t)
	w.connect()
	o := w.order()

	_, err := w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET payment_deadline = now() - interval '1 second' WHERE id = $1`, o.ID)
	require.NoError(t, err)

	_, err = w.Pay.ExpireUnpaid(w.Ctx)
	require.NoError(t, err)

	got := w.reload(o)
	require.Equal(t, entity.OrderExpired, got.Status)
	require.Equal(t, entity.ErrorPaymentExpired, *got.ErrorCode)
	require.Equal(t, entity.PaymentCancelled, got.Payment.Status)
	require.Equal(t, []string{"EXPIRED"}, w.pushes(o))

	c, _ := w.GW.Charge(o.Payment.ExternalID)
	require.True(t, c.Expired, "the payment page is closed too")

	_, err = w.Pay.Retry(w.Ctx, w.Rina.ID, o.ID)
	require.ErrorIs(t, err, entity.ErrNotPayable)

	// An order within its deadline is left alone.
	fresh := w.order()
	_, err = w.Pay.ExpireUnpaid(w.Ctx)
	require.NoError(t, err)
	require.Equal(t, entity.OrderUnpaid, w.reload(fresh).Status)
}

func TestAPaymentMadeJustBeforeTheDeadlineIsNotLost(t *testing.T) {
	w := setup(t)
	w.connect()
	o := w.order()

	// Paid, but the notification never arrived; the deadline passes.
	w.GW.Settle(o.Payment.ExternalID, gateway.StatePaid, "E_WALLET")

	_, err := w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET payment_deadline = now() - interval '1 second' WHERE id = $1`, o.ID)
	require.NoError(t, err)

	_, err = w.Pay.ExpireUnpaid(w.Ctx)
	require.NoError(t, err)

	got := w.reload(o)
	require.Equal(t, entity.OrderCreated, got.Status, "the gateway is asked once more before giving up")
	require.Equal(t, entity.PaymentPaid, got.Payment.Status)
}

func TestPollingFindsPaymentsWhoseNotificationNeverCame(t *testing.T) {
	w := setup(t)
	w.connect()
	o := w.order()

	w.GW.Settle(o.Payment.ExternalID, gateway.StatePaid, "BANK_TRANSFER")

	_, err := w.Pool.Exec(w.Ctx, `UPDATE payments SET checked_at = now() - interval '1 minute'`)
	require.NoError(t, err)

	_, err = w.Pay.Poll(w.Ctx)
	require.NoError(t, err)

	require.Equal(t, entity.OrderCreated, w.reload(o).Status)

	// A pending one is only touched, so it is not asked about again at once.
	other := w.order()

	_, err = w.Pool.Exec(w.Ctx, `UPDATE payments SET checked_at = now() - interval '1 minute' WHERE order_id = $1`, other.ID)
	require.NoError(t, err)

	_, err = w.Pay.Poll(w.Ctx)
	require.NoError(t, err)

	require.Equal(t, entity.OrderUnpaid, w.reload(other).Status)

	var recent bool
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT checked_at > now() - interval '5 seconds' FROM payments WHERE order_id = $1`, other.ID).Scan(&recent))
	require.True(t, recent)
}

func TestTheCustomerCanCancelBeforePaying(t *testing.T) {
	w := setup(t)
	w.connect()
	o := w.order()

	cancelled, err := w.Order.Cancel(w.Ctx, w.Rina.ID, o.ID)
	require.NoError(t, err)
	require.Equal(t, entity.OrderCancelled, cancelled.Status)
	require.Equal(t, entity.PaymentCancelled, cancelled.Payment.Status)

	c, _ := w.GW.Charge(o.Payment.ExternalID)
	require.True(t, c.Expired, "the payment page is closed")
	require.Empty(t, w.POS.Cancelled, "the merchant never heard of it")
	require.Empty(t, w.pushes(o), "no push for what the customer did themselves")

	again, err := w.Order.Cancel(w.Ctx, w.Rina.ID, o.ID)
	require.NoError(t, err)
	require.Equal(t, entity.OrderCancelled, again.Status)
}

func TestAPaymentThatArrivesAfterCancellationIsRefunded(t *testing.T) {
	w := setup(t)
	w.connect()
	o := w.order()
	p := o.Payment

	_, err := w.Order.Cancel(w.Ctx, w.Rina.ID, o.ID)
	require.NoError(t, err)

	// The customer had already paid in their banking app.
	w.GW.Settle(p.ExternalID, gateway.StatePaid, "QRIS")
	require.NoError(t, w.notify(p))

	got := w.reload(o)
	require.Equal(t, entity.OrderCancelled, got.Status, "the order stays cancelled")
	require.Equal(t, entity.PaymentPaid, got.Payment.Status, "but the money is recorded")

	more, err := w.Pay.Refund(w.Ctx)
	require.NoError(t, err)
	require.False(t, more)

	got = w.reload(o)
	require.Equal(t, entity.RefundDone, *got.Payment.RefundStatus)
	require.Len(t, w.GW.Refunded, 1)
	require.EqualValues(t, 15929, w.GW.Refunded[0].AmountRupiah, "the whole charge, fee included")
	require.Contains(t, w.pushes(o), "REFUNDED")

	_, err = w.Pay.Refund(w.Ctx)
	require.NoError(t, err)
	require.Len(t, w.GW.Refunded, 1, "never refunded twice")
}

func TestARefusedPaidOrderIsRefunded(t *testing.T) {
	for name, tc := range map[string]func(w *world, o *entity.Order){
		"the POS refuses it": func(w *world, _ *entity.Order) {
			w.POS.SubmitErr = testutil.RealError(w.T, "submit-closed")
		},
		"the merchant rejects it": func(w *world, o *entity.Order) {
			w.send()
			w.applyPOS(o, entity.OrderRejected)
		},
		"the merchant cancels it": func(w *world, o *entity.Order) {
			w.send()
			w.applyPOS(o, entity.OrderCancelled)
		},
		"the merchant never answers": func(w *world, o *entity.Order) {
			w.send()
			w.applyPOS(o, entity.OrderExpired)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := setup(t)
			w.connect()
			o := w.pay(w.order())

			tc(w, o)
			w.send()

			_, err := w.Pay.Refund(w.Ctx)
			require.NoError(t, err)

			got := w.reload(o)
			require.Equal(t, entity.RefundDone, *got.Payment.RefundStatus)
			require.Len(t, w.GW.Refunded, 1)
			require.Contains(t, w.pushes(o), "REFUNDED")

			var owed int
			require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT count(*) FROM fee_ledger`).Scan(&owed))
			require.Zero(t, owed, "the platform earns nothing on an order that did not happen")
		})
	}
}

// applyPOS changes an order at the POS and lets the event reach us.
func (w *world) applyPOS(o *entity.Order, status string) {
	w.T.Helper()

	got := w.reload(o)
	version := w.POS.Set(got.PlatformOrderID, status)

	require.NoError(w.T, w.Order.ApplyEvent(w.Ctx, version, mustJSON(map[string]any{
		"platformOrderId": got.PlatformOrderID, "posOrderId": *got.POSOrderID, "status": status,
	})))
}

func TestAFulfilledOrderEarnsTheFees(t *testing.T) {
	w := setup(t)
	w.connect()

	o := w.pay(w.order())
	w.send()
	w.applyPOS(o, entity.OrderAccepted)

	var owed int
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT count(*) FROM fee_ledger`).Scan(&owed))
	require.Zero(t, owed, "nothing is owed until the order is completed")

	w.applyPOS(o, entity.OrderCompleted)
	w.applyPOS(o, entity.OrderCompleted)

	st, err := w.Pay.FeeStatement(w.Ctx, w.Merchant.ID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, st.Orders)
	require.Equal(t, quoted, st.SalesMinor)
	require.EqualValues(t, 38900, st.CustomerFeeMinor, "2.5% the customer paid on top")
	require.EqualValues(t, 38900, st.MerchantFeeMinor, "2.5% the merchant owes")
	require.EqualValues(t, 77800, st.TotalOwedMinor, "5% of Rp 15.540, rounded to whole rupiah")
	require.Len(t, st.Entries, 2)

	_, err = w.Pay.Refund(w.Ctx)
	require.NoError(t, err)
	require.Empty(t, w.GW.Refunded, "a completed order is never refunded")

	empty, err := w.Pay.FeeStatement(w.Ctx, w.Merchant.ID, time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	require.Zero(t, empty.Orders)
	require.Empty(t, empty.Entries)
}

func TestPayAtPickupOwesTheMerchantFeeOnly(t *testing.T) {
	w := setup(t)
	w.connect()

	req := w.request()
	req.Payment = entity.PaymentModePayAtPick

	o, _, err := w.Order.Checkout(w.Ctx, w.Rina, key(), req)
	require.NoError(t, err)
	require.Nil(t, o.Payment)
	require.Equal(t, "PAY_AT_PICKUP", w.POS.Submitted[0].Payment["mode"])

	w.applyPOS(o, entity.OrderCompleted)
	w.applyPOS(o, entity.OrderCompleted)

	rows, err := w.Pool.Query(w.Ctx, `SELECT kind, source, amount_minor, payment_id IS NULL FROM fee_ledger`)
	require.NoError(t, err)

	defer rows.Close()

	var n int

	for rows.Next() {
		var (
			kind, source string
			amount       int64
			noPayment    bool
		)

		require.NoError(t, rows.Scan(&kind, &source, &amount, &noPayment))
		require.Equal(t, "MERCHANT_FEE", kind, "nothing is added to what the customer paid in cash")
		require.Equal(t, "PICKUP", source)
		require.EqualValues(t, 38900, amount, "2.5% of Rp 15.540, to the rupiah")
		require.True(t, noPayment)

		n++
	}

	require.Equal(t, 1, n, "completing twice owes once")
}

func TestThePickupFeeRateIsTheOneInForceWhenOrdered(t *testing.T) {
	w := setup(t)
	w.connect()

	req := w.request()
	req.Payment = entity.PaymentModePayAtPick

	o, _, err := w.Order.Checkout(w.Ctx, w.Rina, key(), req)
	require.NoError(t, err)

	var bps int
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT merchant_fee_bps FROM merchant_orders WHERE id = $1`, o.ID).Scan(&bps))
	require.Equal(t, 250, bps, "recorded when the order was placed")

	// The platform lowers its fee later; this order keeps the rate it was placed under.
	_, err = w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET merchant_fee_bps = 100 WHERE id = $1`, o.ID)
	require.NoError(t, err)

	w.applyPOS(o, entity.OrderCompleted)

	var owed int64
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT amount_minor FROM fee_ledger`).Scan(&owed))
	require.EqualValues(t, 15500, owed, "1% of Rp 15.540, rounded to the rupiah")

	// A zero rate owes nothing, and no empty entry is kept.
	zero := w.Customer(t, "uid-zero", "Zed", "")
	other, _, err := w.Order.Checkout(w.Ctx, zero, key(), req)
	require.NoError(t, err)

	_, err = w.Pool.Exec(w.Ctx, `UPDATE merchant_orders SET merchant_fee_bps = 0 WHERE id = $1`, other.ID)
	require.NoError(t, err)

	w.applyPOS(other, entity.OrderCompleted)

	var entries int
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT count(*) FROM fee_ledger`).Scan(&entries))
	require.Equal(t, 1, entries)
}

func TestARefundTheGatewayRefusesIsLeftToTheMerchant(t *testing.T) {
	w := setup(t)
	w.connect()
	o := w.pay(w.order())

	w.POS.SubmitErr = testutil.RealError(t, "submit-closed")
	w.send()

	w.GW.RefundErr = &gateway.Error{Status: 412, Code: "MIDTRANS_412", Message: "Merchant cannot modify the status of the transaction"}

	_, err := w.Pay.Refund(w.Ctx)
	require.NoError(t, err)

	got := w.reload(o)
	require.Equal(t, entity.RefundFailed, *got.Payment.RefundStatus)
	require.Contains(t, w.pushes(o), "REFUND_FAILED", "the customer is told the merchant will return the money")

	_, err = w.Pay.Refund(w.Ctx)
	require.NoError(t, err)
	require.Len(t, w.GW.Refunded, 1, "a refusal is final, not retried forever")
}

func TestARefundThatFailsForNowIsTriedAgain(t *testing.T) {
	w := setup(t)
	w.connect()
	o := w.pay(w.order())

	w.POS.SubmitErr = testutil.RealError(t, "submit-closed")
	w.send()

	w.GW.RefundErr = &gateway.Error{Status: http.StatusServiceUnavailable, Code: "BUSY"}

	_, err := w.Pay.Refund(w.Ctx)
	require.NoError(t, err)
	require.Equal(t, entity.RefundPending, *w.reload(o).Payment.RefundStatus)

	_, err = w.Pay.Refund(w.Ctx)
	require.NoError(t, err)
	require.Len(t, w.GW.Refunded, 1, "it waits before the next try")

	w.GW.RefundErr = nil

	_, err = w.Pool.Exec(w.Ctx, `UPDATE payments SET refund_available_at = now()`)
	require.NoError(t, err)

	_, err = w.Pay.Refund(w.Ctx)
	require.NoError(t, err)
	require.Equal(t, entity.RefundDone, *w.reload(o).Payment.RefundStatus)
	require.Equal(t, w.GW.Refunded[0].Key, w.GW.Refunded[1].Key, "the same key, so the gateway cannot refund twice")
}

func TestPayingTwiceRefundsTheSecondPayment(t *testing.T) {
	w := setup(t)
	w.connect()
	o := w.order()
	first := o.Payment

	// The first attempt fails in the app, the customer retries, then the first one is paid anyway.
	w.GW.Settle(first.ExternalID, gateway.StateFailed, "")
	require.NoError(t, w.notify(first))

	retried, err := w.Pay.Retry(w.Ctx, w.Rina.ID, o.ID)
	require.NoError(t, err)

	w.GW.Settle(retried.Payment.ExternalID, gateway.StatePaid, "QRIS")
	require.NoError(t, w.notify(retried.Payment))

	time.Sleep(10 * time.Millisecond)
	w.GW.Settle(first.ExternalID, gateway.StatePaid, "QRIS")
	require.NoError(t, w.notify(first))

	_, err = w.Pay.Refund(w.Ctx)
	require.NoError(t, err)

	require.Len(t, w.GW.Refunded, 1)
	require.Equal(t, "inv-"+first.ExternalID, w.GW.Refunded[0].ChargeID, "the later payment is the one returned")
	require.Equal(t, entity.OrderCreated, w.reload(o).Status, "the order carries on with the first payment")
}

func TestOnlinePaymentIsOnlyOfferedWhereTheMerchantHasAGateway(t *testing.T) {
	w := setup(t)

	_, _, err := w.Order.Checkout(w.Ctx, w.Rina, key(), w.request())
	require.ErrorIs(t, err, ordering.ErrOnlineUnavailable)
	require.False(t, w.Outlet("kiosk-1").OnlinePayment)

	w.connect()
	require.True(t, w.Outlet("kiosk-1").OnlinePayment)

	require.NoError(t, w.Pay.RemoveGateway(w.Ctx, w.Merchant.ID))
	require.False(t, w.Outlet("kiosk-1").OnlinePayment)

	_, _, err = w.Order.Checkout(w.Ctx, w.Rina, key(), w.request())
	require.ErrorIs(t, err, ordering.ErrOnlineUnavailable)

	// Paying at pickup is unaffected.
	req := w.request()
	req.Payment = ""
	_, _, err = w.Order.Checkout(w.Ctx, w.Rina, key(), req)
	require.NoError(t, err)
}

func TestTheMerchantIsAskedForThePriceBeforeAnyoneIsCharged(t *testing.T) {
	w := setup(t)
	w.connect()

	// Prices moved since the customer saw them.
	req := w.request()
	wrong := int64(1)
	req.ExpectedTotalMinor = &wrong

	_, _, err := w.Order.Checkout(w.Ctx, w.Rina, key(), req)

	var changed *ordering.PriceChangedError

	require.ErrorAs(t, err, &changed)
	require.Equal(t, quoted, changed.TotalMinor)
	require.Empty(t, w.GW.Charges, "nobody is asked to pay a stale price")

	// The merchant cannot take the order at all.
	w.POS.QuoteErr = testutil.RealError(t, "submit-closed")

	_, _, err = w.Order.Checkout(w.Ctx, w.Rina, key(), w.request())

	var refused *ordering.RefusedError

	require.ErrorAs(t, err, &refused)
	require.Equal(t, "OUTLET_CLOSED", refused.Code)
	require.Empty(t, w.GW.Charges)

	// The merchant cannot be reached: nothing is stored or charged.
	w.POS.QuoteErr = nil
	w.POS.Down = true

	_, _, err = w.Order.Checkout(w.Ctx, w.Rina, key(), w.request())
	require.ErrorIs(t, err, ordering.ErrMerchantUnreachable)
	require.Empty(t, w.GW.Charges)

	var orders int
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT count(*) FROM merchant_orders`).Scan(&orders))
	require.Zero(t, orders)
}

func TestAnOnlineCheckoutRepeatedWithTheSameKeyChargesOnce(t *testing.T) {
	w := setup(t)
	w.connect()

	first, created, err := w.Order.Checkout(w.Ctx, w.Rina, "once", w.request())
	require.NoError(t, err)
	require.True(t, created)

	// Prices moved meanwhile: a replay still answers with the order it made.
	w.POS.QuoteTotal = 9999999

	again, created, err := w.Order.Checkout(w.Ctx, w.Rina, "once", w.request())
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, first.ID, again.ID)
	require.Equal(t, first.Payment.ID, again.Payment.ID)
	require.Len(t, w.GW.Charges, 1)

	different := w.request()
	different.Items[0].Quantity = 3
	_, _, err = w.Order.Checkout(w.Ctx, w.Rina, "once", different)
	require.ErrorIs(t, err, entity.ErrIdempotency)
}

func TestAnUnpaidOrderCountsAsOpen(t *testing.T) {
	w := setup(t)
	w.connect()

	for i := 0; i < entity.MaxOpenOrders; i++ {
		w.order()
	}

	_, _, err := w.Order.Checkout(w.Ctx, w.Rina, key(), w.request())
	require.ErrorIs(t, err, entity.ErrTooMany, "unpaid orders cannot be used to flood")
}

func TestConnectingAGateway(t *testing.T) {
	w := setup(t)

	view := w.connect()
	require.Equal(t, "XENDIT", view.Provider)
	require.True(t, view.Active)
	require.Equal(t, "https://api.test/v1/integrations/payments/xendit/"+view.ID, view.CallbackURL, "what the merchant enters in the gateway's dashboard")

	var sealed string
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT credentials_enc FROM merchant_gateways`).Scan(&sealed))
	require.NotContains(t, sealed, "xnd_development_secret", "keys are encrypted at rest")
	require.NotContains(t, sealed, testutil.CallbackToken)

	// Replacing keeps the id, so open payments keep working.
	again, err := w.Pay.SetGateway(w.Ctx, w.Merchant.ID, payment.GatewayInput{Provider: "MIDTRANS", Secret: "SB-Mid-server-x", Sandbox: true})
	require.NoError(t, err)
	require.Equal(t, view.ID, again.ID)
	require.Equal(t, "MIDTRANS", again.Provider)
	require.True(t, again.Sandbox)

	for name, in := range map[string]payment.GatewayInput{
		"unknown provider":       {Provider: "paypal", Secret: "x"},
		"no secret":              {Provider: "MIDTRANS"},
		"xendit without a token": {Provider: "XENDIT", Secret: "x"},
		"very long secret":       {Provider: "MIDTRANS", Secret: string(make([]byte, 400))},
	} {
		_, err := w.Pay.SetGateway(w.Ctx, w.Merchant.ID, in)
		require.ErrorIs(t, err, payment.ErrInvalid, name)
	}

	w.GW.CheckErr = &gateway.Error{Status: 401, Code: "INVALID_API_KEY"}
	_, err = w.Pay.SetGateway(w.Ctx, w.Merchant.ID, payment.GatewayInput{Provider: "MIDTRANS", Secret: "wrong"})
	require.ErrorIs(t, err, payment.ErrUnverified, "keys that do not work are not saved")

	_, err = w.Pay.SetGateway(w.Ctx, "00000000-0000-0000-0000-000000000000", payment.GatewayInput{Provider: "MIDTRANS", Secret: "x"})
	require.Error(t, err)
}

func mustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}

	return raw
}
