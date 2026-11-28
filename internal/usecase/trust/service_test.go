package trust_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/testutil"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/trust"
)

type fixture struct {
	*testutil.Env
	Svc    *trust.Service
	Rina   entity.Customer
	Budi   entity.Customer
	Outlet string
}

func setup(t *testing.T) *fixture {
	t.Helper()

	e := testutil.NewEnv(t)
	k := e.PublishKiosk(t)

	return &fixture{
		Env: e, Svc: trust.New(e.Store, testutil.SilentLogger{}), Outlet: k.Outlet.ID,
		Rina: e.Customer(t, "uid-rina", "Rina", "+628123456789"),
		Budi: e.Customer(t, "uid-budi", "Budi", ""),
	}
}

func (f *fixture) record(c entity.Customer, kind, source string, daysAgo int, evidence map[string]any) {
	f.T.Helper()

	raw, _ := json.Marshal(evidence)
	merchant := f.Merchant.ID

	require.NoError(f.T, f.Store.RecordTrustEvent(f.Ctx, entity.TrustEvent{
		CustomerID: c.ID, Type: kind, Source: source, MerchantID: &merchant,
		Evidence: raw, CreatedAt: time.Now().AddDate(0, 0, -daysAgo),
	}))
}

func TestAReportExplainsTheScore(t *testing.T) {
	f := setup(t)

	for i := 0; i < 5; i++ {
		f.record(f.Rina, entity.EventOrderCompleted, entity.SourceSystem, 10+i, nil)
	}

	f.record(f.Rina, entity.EventNoShow, entity.SourceVendor, 2, map[string]any{"corroborated": true})
	f.record(f.Rina, entity.EventNoShow, entity.SourceVendor, 4, map[string]any{"corroborated": true})

	report, err := f.Svc.Report(f.Ctx, f.Rina.ID)
	require.NoError(t, err)

	require.Equal(t, 7, report.Profile.FinishedOrders)
	require.Equal(t, 5, report.Profile.CompletedOrders)
	require.NotEmpty(t, report.Profile.Reasons)
	require.Len(t, report.Timeline, 7)
	require.Equal(t, entity.EventNoShow, report.Timeline[0].Type, "newest first")
	require.Equal(t, entity.TierWatch, report.Profile.Tier, "two corroborated no-shows in a week among seven orders is a pattern forming")
	require.True(t, report.Policy.PayAtPickup)
}

func TestEvaluatingStoresTheProfileAndASweepKeepsItFresh(t *testing.T) {
	f := setup(t)
	f.record(f.Rina, entity.EventOrderCompleted, entity.SourceSystem, 1, nil)

	more, err := f.Svc.Sweep(f.Ctx)
	require.NoError(t, err)
	require.False(t, more)

	var finished int
	require.NoError(t, f.Pool.QueryRow(f.Ctx, `SELECT finished_orders FROM customer_trust_profile WHERE customer_id = $1`, f.Rina.ID).Scan(&finished))
	require.Equal(t, 1, finished)

	t.Run("nothing to do until something new happens", func(t *testing.T) {
		ids, err := f.Store.CustomersToScore(f.Ctx, 10)
		require.NoError(t, err)
		require.Empty(t, ids)

		f.record(f.Rina, entity.EventOrderCompleted, entity.SourceSystem, 0, nil)

		ids, err = f.Store.CustomersToScore(f.Ctx, 10)
		require.NoError(t, err)
		require.Equal(t, []string{f.Rina.ID}, ids)

		_, err = f.Svc.Sweep(f.Ctx)
		require.NoError(t, err)
		require.NoError(t, f.Pool.QueryRow(f.Ctx, `SELECT finished_orders FROM customer_trust_profile WHERE customer_id = $1`, f.Rina.ID).Scan(&finished))
		require.Equal(t, 2, finished)
	})
}

func TestTheRulesCanBeTunedWithoutADeploy(t *testing.T) {
	f := setup(t)

	for i := 0; i < 4; i++ {
		f.record(f.Rina, entity.EventOrderCompleted, entity.SourceSystem, 40+i, nil)
	}

	f.record(f.Rina, entity.EventNoShow, entity.SourceVendor, 1, map[string]any{"corroborated": true})

	before, err := f.Svc.Report(f.Ctx, f.Rina.ID)
	require.NoError(t, err)

	_, err = f.Pool.Exec(f.Ctx, `INSERT INTO trust_config (key, value) VALUES
		('weights', '{"NO_SHOW": {"points": 200, "halfLifeDays": 60}}'),
		('settings', '{"watchAt": 5}')`)
	require.NoError(t, err)

	// A fresh service sees the new rules at once; a running one within a minute.
	after, err := trust.New(f.Store, testutil.SilentLogger{}).Report(f.Ctx, f.Rina.ID)
	require.NoError(t, err)

	require.Greater(t, after.Profile.Score, before.Profile.Score)
	require.Equal(t, 5.0, after.Config["watchAt"])
	require.InDelta(t, 60, after.Config["vendorCap"], 0.001, "settings that were not overridden keep their default")
}

func TestABrokenConfigRowIsIgnored(t *testing.T) {
	f := setup(t)

	_, err := f.Pool.Exec(f.Ctx, `INSERT INTO trust_config (key, value) VALUES ('weights', '"not an object"'), ('settings', '[1,2]')`)
	require.NoError(t, err)

	cfg := f.Svc.Config(f.Ctx)
	require.Equal(t, trust.DefaultConfig(), cfg)
}

func TestTheDistributionShowsWhoWouldBeLimited(t *testing.T) {
	f := setup(t)

	for i := 0; i < 6; i++ {
		f.record(f.Rina, entity.EventOrderCompleted, entity.SourceSystem, 5+i, nil)
	}

	for i := 0; i < 3; i++ {
		f.record(f.Budi, entity.EventOrderCompleted, entity.SourceSystem, 30+i, nil)
		f.record(f.Budi, entity.EventNoShow, entity.SourceVendor, 1+i, map[string]any{"corroborated": true})
	}

	d, err := f.Svc.Distribution(f.Ctx)
	require.NoError(t, err)

	require.Equal(t, 2, d.Customers)
	require.Equal(t, 1, d.Tiers[entity.TierGood])
	require.Equal(t, 1, d.WouldLimit)
	require.False(t, d.Truncated)
	require.GreaterOrEqual(t, d.ScoreP99, d.ScoreP50)

	var n int
	require.NoError(t, f.Pool.QueryRow(f.Ctx, `SELECT count(*) FROM customer_trust_profile`).Scan(&n))
	require.Zero(t, n, "looking at the distribution stores nothing")
}

func TestShadowModeNeverChangesAnything(t *testing.T) {
	f := setup(t)
	f.record(f.Rina, entity.EventNoShow, entity.SourceVendor, 1, map[string]any{"corroborated": true})

	before, err := f.Store.OpenOrderCount(f.Ctx, f.Rina.ID)
	require.NoError(t, err)

	f.Svc.Shadow(context.Background(), f.Rina, f.Outlet)

	after, err := f.Store.OpenOrderCount(f.Ctx, f.Rina.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)

	var tier string
	require.NoError(t, f.Pool.QueryRow(f.Ctx, `SELECT tier FROM customer_trust_profile WHERE customer_id = $1`, f.Rina.ID).Scan(&tier))
	require.Equal(t, entity.TierNew, tier, "it records the profile and nothing else")
}
