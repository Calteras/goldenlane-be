package payment_test

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/testutil"
	"github.com/Calteras/streetfood-customer-api/pkg/signing"
)

const (
	reservePath = "/v1/integrations/pos/fees/reserve"
	pendingPath = "/v1/integrations/pos/fees/pending"
)

// signed is a request the POS signs with its link's credentials.
type signed struct{ key, secret, ts, method, path string }

func (w *world) pos(method, path string) signed {
	return signed{key: w.KeyID, secret: testutil.SecretText, ts: strconv.FormatInt(time.Now().Unix(), 10), method: method, path: path}
}

func (w *world) reserve(ref string) (entity.FeeReservation, error) {
	return w.reserveAs(w.pos("POST", reservePath), `{"billingRef":"`+ref+`"}`)
}

func (w *world) reserveAs(s signed, body string) (entity.FeeReservation, error) {
	return w.Integration.ReserveFees(context.Background(), s.key, s.ts, signing.Sign(s.secret, s.ts, s.method, s.path, []byte(body)), s.method, s.path, []byte(body))
}

func (w *world) pending() entity.FeeReservation {
	w.T.Helper()

	s := w.pos("GET", pendingPath)
	out, err := w.Integration.PendingFees(context.Background(), s.key, s.ts, signing.Sign(s.secret, s.ts, s.method, s.path, nil), s.method, s.path, nil)
	require.NoError(w.T, err)

	return out
}

// settled is the event the POS sends once the subscription payment went through.
func (w *world) settled(ref string, amount int64) {
	w.T.Helper()

	require.NoError(w.T, w.Send(testutil.Event{
		ID: "settled-" + ref + "-" + key(), Type: "billing.fees.settled", At: time.Now(),
		Data: map[string]any{"billingRef": ref, "orderId": "sub-order-1", "invoiceNumber": "INV-2026-000001", "amountMinor": amount},
	}))
	w.Drain()
}

// owe completes one online order (77.800 owed) and one pay-at-pickup order (38.900 owed).
func (w *world) owe() {
	w.T.Helper()

	online := w.pay(w.order())
	w.send()
	w.applyPOS(online, entity.OrderCompleted)

	req := w.request()
	req.Payment = entity.PaymentModePayAtPick

	pickup, _, err := w.Order.Checkout(w.Ctx, w.Rina, key(), req)
	require.NoError(w.T, err)
	w.applyPOS(pickup, entity.OrderCompleted)
}

func TestWhatAMerchantOwesIsShownByKind(t *testing.T) {
	w := setup(t)
	w.connect()
	w.owe()

	p := w.pending()
	require.EqualValues(t, 116700, p.TotalMinor, "online 5% (Rp 778) plus pay-at-pickup 2.5% (Rp 389)")
	require.Len(t, p.Lines, 2)
	require.Equal(t, "ONLINE_ORDERS", p.Lines[0].Code)
	require.EqualValues(t, 77800, p.Lines[0].AmountMinor)
	require.Equal(t, 1, p.Lines[0].Orders)
	require.Equal(t, "PICKUP_ORDERS", p.Lines[1].Code)
	require.EqualValues(t, 38900, p.Lines[1].AmountMinor)

	again := w.pending()
	require.Equal(t, p.TotalMinor, again.TotalMinor, "looking does not reserve")

	var reserved int
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT count(*) FROM fee_ledger WHERE billing_ref IS NOT NULL`).Scan(&reserved))
	require.Zero(t, reserved)
}

func TestReservingFeesForABillingPeriod(t *testing.T) {
	w := setup(t)
	w.connect()
	w.owe()

	first, err := w.reserve("sub-1:2026-12-01")
	require.NoError(t, err)
	require.EqualValues(t, 116700, first.TotalMinor)
	require.Equal(t, "sub-1:2026-12-01", first.BillingRef)
	require.Len(t, first.Lines, 2)

	again, err := w.reserve("sub-1:2026-12-01")
	require.NoError(t, err)
	require.Equal(t, first.TotalMinor, again.TotalMinor, "asking again for the same period gives the same answer")

	// More fees accrue while the charge is in flight.
	w.owe()

	same, err := w.reserve("sub-1:2026-12-01")
	require.NoError(t, err)
	require.Equal(t, first.TotalMinor, same.TotalMinor, "what was set aside does not grow under a charge already made")

	next, err := w.reserve("sub-1:2027-01-01")
	require.NoError(t, err)
	require.EqualValues(t, 116700, next.TotalMinor, "the next period gets only what the first did not take")

	other, err := w.reserve("sub-1:2027-02-01")
	require.NoError(t, err)
	require.Zero(t, other.TotalMinor)
	require.Empty(t, other.Lines)
}

func TestSettlingCollectsExactlyWhatWasReserved(t *testing.T) {
	w := setup(t)
	w.connect()
	w.owe()

	res, err := w.reserve("sub-1:2026-12-01")
	require.NoError(t, err)

	// A report for a period nobody reserved, or a made-up one, changes nothing.
	w.settled("sub-9:2026-12-01", 1)
	require.EqualValues(t, 116700, w.pending().TotalMinor)

	w.settled("sub-1:2026-12-01", res.TotalMinor)
	require.Zero(t, w.pending().TotalMinor, "collected")

	w.settled("sub-1:2026-12-01", res.TotalMinor)
	require.Zero(t, w.pending().TotalMinor, "a repeated report changes nothing")

	// The same period can never be charged twice.
	again, err := w.reserve("sub-1:2026-12-01")
	require.NoError(t, err)
	require.True(t, again.Collected)
	require.Zero(t, again.TotalMinor)

	// Fees that accrue afterwards belong to the next period.
	w.owe()
	still, err := w.reserve("sub-1:2026-12-01")
	require.NoError(t, err)
	require.True(t, still.Collected)
	require.Zero(t, still.TotalMinor)

	next, err := w.reserve("sub-1:2027-01-01")
	require.NoError(t, err)
	require.EqualValues(t, 116700, next.TotalMinor)

	st, err := w.Pay.FeeStatement(w.Ctx, w.Merchant.ID, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 233400, st.TotalOwedMinor)
	require.EqualValues(t, 116700, st.SettledMinor)
	require.EqualValues(t, 116700, st.OutstandingMinor)
	require.NotNil(t, st.Entries[0].SettledAt)
}

func TestAReservationNobodyPaidIsReleased(t *testing.T) {
	w := setup(t)
	w.connect()
	w.owe()

	_, err := w.reserve("sub-1:2026-12-01")
	require.NoError(t, err)

	// The subscription lapsed and the charge never went through. Three weeks later the fees are free again.
	none, err := w.reserve("sub-1:2027-01-01")
	require.NoError(t, err)
	require.Zero(t, none.TotalMinor, "still set aside for the first period")

	_, err = w.Pool.Exec(w.Ctx, `UPDATE fee_ledger SET reserved_at = now() - interval '22 days'`)
	require.NoError(t, err)

	next, err := w.reserve("sub-1:2027-01-01")
	require.NoError(t, err)
	require.EqualValues(t, 116700, next.TotalMinor)
}

func TestOnlyThePOSOfTheMerchantCanAskForOrReportFees(t *testing.T) {
	w := setup(t)
	w.connect()
	w.owe()

	body := `{"billingRef":"x"}`

	good := w.pos("POST", reservePath)
	bad := good
	bad.secret = "sfs_wrong"
	_, err := w.reserveAs(bad, body)
	require.ErrorIs(t, err, entity.ErrUnauthorized)

	unknown := good
	unknown.key = "sfk_ffffffffffffffffffffffff"
	_, err = w.reserveAs(unknown, body)
	require.ErrorIs(t, err, entity.ErrUnauthorized)

	stale := good
	stale.ts = strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10)
	_, err = w.reserveAs(stale, body)
	require.ErrorIs(t, err, entity.ErrUnauthorized)

	// A signature for one path cannot be replayed on another.
	_, err = w.Integration.ReserveFees(w.Ctx, good.key, good.ts, signing.Sign(good.secret, good.ts, "POST", pendingPath, []byte(body)), "POST", reservePath, []byte(body))
	require.ErrorIs(t, err, entity.ErrUnauthorized)

	for _, b := range []string{``, `{}`, `{"billingRef":""}`, `nope`} {
		_, err = w.reserveAs(good, b)
		require.ErrorIs(t, err, entity.ErrBadRequest, b)
	}

	require.EqualValues(t, 116700, w.pending().TotalMinor, "nothing was reserved by the refused calls")
}

func TestOneMerchantCannotSeeOrSettleAnothersFees(t *testing.T) {
	w := setup(t)
	w.connect()
	w.owe()

	res, err := w.reserve("sub-1:2026-12-01")
	require.NoError(t, err)

	otherKey := "sfk_" + "abcdefabcdefabcdefabcdef"
	w.AddMerchant("Kopi Senja", "tenant-2", otherKey, "sfs_other_secret")

	s := signed{key: otherKey, secret: "sfs_other_secret", ts: strconv.FormatInt(time.Now().Unix(), 10), method: "POST", path: reservePath}
	theirs, err := w.reserveAs(s, `{"billingRef":"sub-1:2026-12-01"}`)
	require.NoError(t, err)
	require.Zero(t, theirs.TotalMinor, "the same period name, a different merchant: nothing owed")

	// Their POS reports a payment for our period name: our entries are untouched.
	require.NoError(t, w.SendAs(otherKey, "sfs_other_secret", testutil.Event{
		ID: "forged", Type: "billing.fees.settled", At: time.Now(),
		Data: map[string]any{"billingRef": "sub-1:2026-12-01", "invoiceNumber": "INV-X", "amountMinor": res.TotalMinor},
	}))
	w.Drain()

	var settled int
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT count(*) FROM fee_ledger WHERE settled_at IS NOT NULL`).Scan(&settled))
	require.Zero(t, settled)
}

func TestTheOperatorCanSettleByHand(t *testing.T) {
	w := setup(t)
	w.connect()
	w.owe()

	_, err := w.Pay.SettleFeesManually(w.Ctx, w.Merchant.ID, time.Now().Add(time.Hour), "")
	require.ErrorIs(t, err, entity.ErrBadRequest, "a note about how it was collected is required")

	_, err = w.Pay.SettleFeesManually(w.Ctx, "00000000-0000-0000-0000-000000000000", time.Now().Add(time.Hour), "bank transfer")
	require.ErrorIs(t, err, entity.ErrNotFound)

	// Fees set aside for a charge in flight are left to that charge.
	_, err = w.reserve("sub-1:2026-12-01")
	require.NoError(t, err)

	out, err := w.Pay.SettleFeesManually(w.Ctx, w.Merchant.ID, time.Now().Add(time.Hour), "Paid by transfer 2026-11-30")
	require.NoError(t, err)
	require.Zero(t, out.Entries)

	_, err = w.Pool.Exec(w.Ctx, `UPDATE fee_ledger SET reserved_at = now() - interval '22 days'`)
	require.NoError(t, err)

	_, err = w.reserve("sub-2:2027-01-01") // releases the first period's entries and sets them aside again
	require.NoError(t, err)

	_, err = w.Pool.Exec(w.Ctx, `UPDATE fee_ledger SET billing_ref = NULL, reserved_at = NULL`)
	require.NoError(t, err)

	out, err = w.Pay.SettleFeesManually(w.Ctx, w.Merchant.ID, time.Now().Add(time.Hour), "Paid by transfer 2026-11-30")
	require.NoError(t, err)
	require.Equal(t, 3, out.Entries)
	require.EqualValues(t, 116700, out.AmountMinor)
	require.Zero(t, w.pending().TotalMinor)

	var note string
	require.NoError(t, w.Pool.QueryRow(w.Ctx, `SELECT settlement_note FROM fee_ledger LIMIT 1`).Scan(&note))
	require.Equal(t, "Paid by transfer 2026-11-30", note)

	// Fees from after the cut-off are not touched.
	w.owe()
	out, err = w.Pay.SettleFeesManually(w.Ctx, w.Merchant.ID, time.Now().Add(-time.Hour), "earlier cut-off")
	require.NoError(t, err)
	require.Zero(t, out.Entries)
}
