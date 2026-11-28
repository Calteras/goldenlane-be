package trust_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/trust"
)

func vendor(id string, completed, noShows, cancelled, rejected int) entity.VendorStat {
	return entity.VendorStat{
		MerchantID: id, Name: id, Completed: completed, NoShows: noShows,
		VendorCancelled: cancelled, VendorRejected: rejected,
	}
}

// peers are four ordinary vendors: about one customer in twenty does not come.
func peers() []entity.VendorStat {
	return []entity.VendorStat{
		vendor("a", 57, 3, 2, 1), vendor("b", 58, 2, 1, 0),
		vendor("c", 56, 4, 3, 2), vendor("d", 59, 1, 2, 1),
	}
}

func flagsOf(stats []entity.VendorStat, id string) []string {
	for _, s := range stats {
		if s.MerchantID == id {
			return s.Flags
		}
	}

	return nil
}

func TestAVendorThatCallsFarMoreNoShowsThanItsPeersStandsOut(t *testing.T) {
	stats := append(peers(), vendor("odd", 30, 30, 1, 0))
	p := trust.Assess(stats, trust.DefaultConfig())

	require.True(t, p.Comparable)
	require.Equal(t, []string{trust.FlagNoShowRate}, flagsOf(stats, "odd"))

	for _, id := range []string{"a", "b", "c", "d"} {
		require.Empty(t, flagsOf(stats, id), id)
	}

	require.Less(t, p.NoShowRate, 0.1, "the median is that of ordinary vendors")
}

func TestOnlyTheNoShowFlagLowersHowMuchAVendorIsBelieved(t *testing.T) {
	stats := append(peers(),
		vendor("noshow", 30, 30, 1, 0),
		vendor("cancels", 20, 0, 30, 0),
		vendor("rejects", 20, 0, 0, 30),
	)
	trust.Assess(stats, trust.DefaultConfig())

	require.Contains(t, flagsOf(stats, "cancels"), trust.FlagCancelRate)
	require.Contains(t, flagsOf(stats, "rejects"), trust.FlagRejectRate)

	factors := trust.Factors(stats, trust.DefaultConfig())
	require.Equal(t, map[string]float64{"noshow": 0.5}, factors,
		"cancelling or rejecting a lot is about the vendor, not about whether it can be believed of customers")
}

func TestNobodyStandsOutWithoutEnoughPeers(t *testing.T) {
	stats := []entity.VendorStat{vendor("a", 30, 3, 0, 0), vendor("odd", 30, 30, 0, 0)}
	p := trust.Assess(stats, trust.DefaultConfig())

	require.False(t, p.Comparable, "a median of two vendors says nothing")
	require.Empty(t, flagsOf(stats, "odd"))
}

func TestAYoungVendorIsNotJudgedOnAHandfulOfOrders(t *testing.T) {
	stats := append(peers(), vendor("new", 4, 4, 0, 0))
	trust.Assess(stats, trust.DefaultConfig())

	require.Empty(t, flagsOf(stats, "new"), "eight orders is too few to compare")
}

func TestAFewUnluckyDaysAreNotEnough(t *testing.T) {
	// Above the floor and the multiple, but only four no-shows: fewer than the five it takes.
	stats := append(peers(), vendor("unlucky", 8, 4, 0, 0))
	cfg := trust.DefaultConfig()
	cfg.VendorMinOrders = 10
	trust.Assess(stats, cfg)

	require.Empty(t, flagsOf(stats, "unlucky"))
}

func TestAHighRateAloneIsNotAnOutlierWhenEveryoneIsHigh(t *testing.T) {
	// A place where customers often do not come: everyone's rate is high, so none stands out.
	stats := []entity.VendorStat{
		vendor("a", 40, 20, 0, 0), vendor("b", 40, 22, 0, 0),
		vendor("c", 40, 19, 0, 0), vendor("d", 40, 24, 0, 0),
	}
	trust.Assess(stats, trust.DefaultConfig())

	for _, s := range stats {
		require.Empty(t, s.Flags, s.MerchantID)
	}
}

func TestAReportFromAVendorThatStandsOutCountsForLess(t *testing.T) {
	// Two no-shows next to four good orders: enough that the score is above zero either way.
	with := func(merchant string) []entity.TrustEvent {
		return append(completed(4, 30), noShow(merchant, 1, true), noShow(merchant, 2, true))
	}

	cfg := trust.DefaultConfig()
	plain := trust.Score(with("odd"), cfg, now)

	cfg.VendorFactors = map[string]float64{"odd": 0.5}
	discounted := trust.Score(with("odd"), cfg, now)
	untouched := trust.Score(with("fine"), cfg, now)

	require.InDelta(t, pointsOf(plain, entity.EventNoShow)/2, pointsOf(discounted, entity.EventNoShow), 0.05)
	require.Equal(t, plain.Score, untouched.Score, "a vendor that is not discounted is believed in full")
	require.Greater(t, plain.Score, 0.0)
	require.Less(t, discounted.Score, plain.Score)
}

func TestAThumbsUpHelpsAndAnUnbackedThumbsDownIsWeak(t *testing.T) {
	base := completed(6, 30)
	cfg := trust.DefaultConfig()

	up := ev(entity.EventVendorThumbsUp, entity.SourceVendor, "m1", 1, nil)
	down := ev(entity.EventVendorThumbsDown, entity.SourceVendor, "m1", 1, map[string]any{"corroborated": false})

	require.Less(t, pointsOf(trust.Score(append(append([]entity.TrustEvent{}, base...), up), cfg, now), entity.EventVendorThumbsUp), 0.0)

	one := trust.Score(append(append([]entity.TrustEvent{}, base...), down), cfg, now)
	require.InDelta(t, 3.0, pointsOf(one, entity.EventVendorThumbsDown), 0.2, "6 points, halved because nothing the platform saw backs it up")

	ns := trust.Score(append(append([]entity.TrustEvent{}, base...), noShow("m1", 1, true)), cfg, now)
	require.Less(t, pointsOf(one, entity.EventVendorThumbsDown), pointsOf(ns, entity.EventNoShow)/5, "far weaker than a corroborated no-show")
}

func TestOneVendorsThumbsDownCannotSinkACustomerEither(t *testing.T) {
	var events []entity.TrustEvent

	events = append(events, completed(6, 30)...)
	for i := 0; i < 15; i++ {
		events = append(events, ev(entity.EventVendorThumbsDown, entity.SourceVendor, "m1", float64(i), map[string]any{"corroborated": false}))
	}

	r := trust.Score(events, trust.DefaultConfig(), now)

	require.LessOrEqual(t, pointsOf(r, entity.EventVendorThumbsDown), trust.DefaultConfig().VendorCap+0.01,
		"fifteen thumbs down from one merchant add no more than its cap")
}
