package trust_test

import (
	"encoding/json"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/trust"
)

var now = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

func ev(kind, source, merchant string, daysAgo float64, extra map[string]any) entity.TrustEvent {
	raw, _ := json.Marshal(extra)

	return entity.TrustEvent{
		CustomerID: "c1", Type: kind, Source: source, MerchantID: &merchant,
		Evidence: raw, CreatedAt: now.Add(-time.Duration(daysAgo * 24 * float64(time.Hour))),
	}
}

func completed(n int, daysAgo float64) []entity.TrustEvent {
	var out []entity.TrustEvent
	for i := 0; i < n; i++ {
		out = append(out, ev(entity.EventOrderCompleted, entity.SourceSystem, "m1", daysAgo+float64(i), nil))
	}

	return out
}

func noShow(merchant string, daysAgo float64, corroborated bool) entity.TrustEvent {
	return ev(entity.EventNoShow, entity.SourceVendor, merchant, daysAgo, map[string]any{"corroborated": corroborated, "paymentMode": "PAY_AT_PICKUP"})
}

func score(events ...entity.TrustEvent) trust.Result {
	return trust.Score(events, trust.DefaultConfig(), now)
}

func TestNobodyStartsAsARisk(t *testing.T) {
	r := score()
	require.Equal(t, entity.TierNew, r.Tier)
	require.Zero(t, r.Score)
	require.Empty(t, r.Reasons)
}

func TestOneBadOrderOnAShortHistoryIsNotAVerdict(t *testing.T) {
	r := score(noShow("m1", 1, true))
	require.Equal(t, entity.TierNew, r.Tier, "fewer than 3 finished orders is neutral")
	require.Less(t, r.Score, 10.0, "and the score is smoothed against a prior of neutral orders")

	r = score(append(completed(1, 5), noShow("m1", 1, true))...)
	require.Equal(t, entity.TierNew, r.Tier, "two finished orders is still too few to judge")

	r = score(append(completed(2, 5), noShow("m1", 1, true))...)
	require.Equal(t, entity.TierGood, r.Tier, "at three, one no-show next to two good orders is not enough to flag")
}

func TestAReliableCustomerIsGood(t *testing.T) {
	r := score(completed(8, 2)...)
	require.Equal(t, entity.TierGood, r.Tier)
	require.Zero(t, r.Score, "good behaviour lowers the score but it never goes below zero")
	require.Equal(t, 8, r.Completed)
	require.Equal(t, entity.EventOrderCompleted, r.Reasons[0].Code)
	require.Less(t, r.Reasons[0].Points, 0.0)
}

func TestRepeatedNoShowsMoveTheTierUp(t *testing.T) {
	// Three corroborated no-shows among eight orders in two weeks.
	events := append(completed(5, 10), noShow("m1", 1, true), noShow("m2", 3, true), noShow("m3", 6, true))
	r := score(events...)
	require.Equal(t, entity.TierWatch, r.Tier)
	require.Greater(t, r.Score, 20.0)
	require.Less(t, r.Score, 50.0)

	var reason entity.TrustReason

	for _, x := range r.Reasons {
		if x.Code == entity.EventNoShow {
			reason = x
		}
	}

	require.Equal(t, 3, reason.Count)
	require.Greater(t, reason.Points, 70.0, "the reasons list what each kind of event added")

	more := append(events, noShow("m4", 8, true), noShow("m5", 9, true), noShow("m6", 10, true))
	require.Equal(t, entity.TierRestricted, score(more...).Tier)
}

func TestOldProblemsFade(t *testing.T) {
	recent := score(append(completed(5, 200), noShow("m1", 1, true))...)
	old := score(append(completed(5, 200), noShow("m1", 120, true))...)

	require.Greater(t, recent.Score, old.Score*3.5, "two half-lives later it counts for about a quarter")

	var no float64

	for _, x := range old.Reasons {
		if x.Code == entity.EventNoShow {
			no = x.Points
		}
	}

	require.InDelta(t, 7.5, no, 0.1, "30 points, halved twice")
}

func pointsOf(r trust.Result, code string) float64 {
	for _, x := range r.Reasons {
		if x.Code == code {
			return x.Points
		}
	}

	return 0
}

func TestAVendorsUnbackedClaimCountsForLess(t *testing.T) {
	base := completed(5, 20)
	backed := score(append(base, noShow("m1", 1, true))...)
	unbacked := score(append(base, noShow("m1", 1, false))...)

	require.InDelta(t, pointsOf(backed, entity.EventNoShow)/2, pointsOf(unbacked, entity.EventNoShow), 0.05)
	require.Less(t, unbacked.Score, backed.Score)
}

func TestOneVendorCannotSinkACustomer(t *testing.T) {
	var oneVendor, manyVendors []entity.TrustEvent

	for i := 0; i < 6; i++ {
		oneVendor = append(oneVendor, noShow("m1", float64(i), true))
		manyVendors = append(manyVendors, noShow("m"+string(rune('a'+i)), float64(i), true))
	}

	base := completed(6, 30)
	capped := score(append(base, oneVendor...)...)
	spread := score(append(base, manyVendors...)...)

	require.Less(t, capped.Score, spread.Score, "six reports from one vendor add no more than its cap")
	require.Equal(t, entity.TierWatch, capped.Tier, "60 points at most from one vendor, then smoothed")
	require.Equal(t, entity.TierRestricted, spread.Tier)
}

func TestAPrepaidNoShowCountsLessThanOneAtPickup(t *testing.T) {
	base := completed(6, 30)
	atPickup := score(append(base, noShow("m1", 1, true))...)
	prepaid := score(append(base, ev(entity.EventNoShow, entity.SourceVendor, "m1", 1, map[string]any{"corroborated": true, "paymentMode": "ONLINE"}))...)

	require.Less(t, prepaid.Score, atPickup.Score, "the vendor was paid")
}

func TestEarlyCancelsOnlyCountWhenItBecomesAHabit(t *testing.T) {
	early := func(n int) []entity.TrustEvent {
		var out []entity.TrustEvent
		for i := 0; i < n; i++ {
			out = append(out, ev(entity.EventCancelBeforeAcc, entity.SourceSystem, "m1", float64(i), nil))
		}

		return out
	}

	few := score(append(completed(10, 2), early(2)...)...)
	require.Zero(t, few.Score, "two changes of mind among twelve orders is normal")

	habit := score(append(completed(3, 2), early(6)...)...)

	var found bool

	for _, x := range habit.Reasons {
		if x.Code == entity.EventCancelBeforeAcc {
			found = true
		}
	}

	require.True(t, found, "six of nine orders cancelled is a pattern")
}

func TestOnlyAPersonCanSuspend(t *testing.T) {
	var events []entity.TrustEvent
	for i := 0; i < 30; i++ {
		events = append(events, noShow("m"+string(rune('a'+i%20)), float64(i%20), true), ev(entity.EventPaymentRefused, entity.SourceVendor, "mx", 1, nil))
	}

	r := score(events...)
	require.Equal(t, entity.TierRestricted, r.Tier, "the highest a score can earn")
}

func TestTheVendorsOwnReasonsNeverAddPoints(t *testing.T) {
	r := score(append(completed(5, 3),
		ev(entity.EventVendorCancelled, entity.SourceVendor, "m1", 1, nil),
		ev(entity.EventVendorRejected, entity.SourceVendor, "m1", 1, nil),
		ev(entity.EventVendorNoResponse, entity.SourceSystem, "m1", 1, nil))...)

	require.Zero(t, r.Score)

	for _, x := range r.Reasons {
		require.NotContains(t, []string{entity.EventVendorCancelled, entity.EventVendorRejected, entity.EventVendorNoResponse}, x.Code)
	}
}

func TestTheSameEventsAlwaysGiveTheSameScore(t *testing.T) {
	events := append(completed(7, 4), noShow("m1", 1, true), noShow("m2", 2, false),
		ev(entity.EventPaymentExpired, entity.SourceSystem, "m1", 3, nil),
		ev(entity.EventLatePickup, entity.SourceSystem, "m3", 5, nil))

	want := score(events...)

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		shuffled := append([]entity.TrustEvent(nil), events...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })

		require.Equal(t, want, score(shuffled...))
	}
}

func TestEventsOutsideTheWindowAreIgnored(t *testing.T) {
	r := score(append(completed(5, 3), noShow("m1", 400, true))...)
	require.Zero(t, r.Score)
}

func TestThePolicyTable(t *testing.T) {
	require.Equal(t, 2, trust.PolicyFor(entity.TierNew).MaxOpenOrders)
	require.Equal(t, entity.MaxOpenOrders, trust.PolicyFor(entity.TierGood).MaxOpenOrders, "good customers are not limited further")
	require.True(t, trust.PolicyFor(entity.TierWatch).VendorMayRequirePrepay)
	require.False(t, trust.PolicyFor(entity.TierRestricted).PayAtPickup)
	require.Zero(t, trust.PolicyFor(entity.TierSuspended).MaxOpenOrders)

	require.Empty(t, trust.Differences(trust.PolicyFor(entity.TierGood), 4, true), "nothing would change for a good customer")
	require.Contains(t, trust.Differences(trust.PolicyFor(entity.TierNew), 0, false), "PHONE_REQUIRED")
	require.Contains(t, trust.Differences(trust.PolicyFor(entity.TierRestricted), 1, true), "OPEN_ORDER_CAP")
	require.Contains(t, trust.Differences(trust.PolicyFor(entity.TierRestricted), 0, true), "PREPAY_REQUIRED")
}
