package discovery_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/testutil"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/discovery"
)

// One degree of latitude is about 111 km, so 0.0027 degrees is roughly 300 m.
const (
	homeLat = -6.2000
	homeLng = 106.8000
	per100m = 0.0009
)

func at(t *testing.T) (*testutil.Env, *discovery.UseCase, time.Time) {
	t.Helper()

	e := testutil.NewEnv(t)

	return e, discovery.New(e.Store), time.Now().Add(-time.Hour)
}

func kiosk(e *testutil.Env, id, name string, metersNorth float64, status string, t0 time.Time) {
	o := testutil.POSOutlet(id, name, 1, homeLat+metersNorth/100*per100m, homeLng, "FIXED")
	o.OnlineStatus = status
	e.Apply(testutil.OutletEvent("ev-"+id, o, t0))
}

func names(list []discovery.Outlet) []string {
	out := make([]string, 0, len(list))
	for _, o := range list {
		out = append(out, o.Name)
	}

	return out
}

func TestNearbyRadiusAndOrder(t *testing.T) {
	e, uc, t0 := at(t)
	kiosk(e, "far", "Far Kiosk", 10000, "OPEN", t0)
	kiosk(e, "remote", "Remote Kiosk", 80000, "OPEN", t0)
	kiosk(e, "mid", "Mid Kiosk", 1500, "OPEN", t0)
	kiosk(e, "near", "Near Kiosk", 300, "OPEN", t0)

	got, err := uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng, RadiusM: 2000})
	require.NoError(t, err)
	require.Equal(t, []string{"Near Kiosk", "Mid Kiosk"}, names(got), "nearest first, the far one outside the radius")
	require.InDelta(t, 300, *got[0].DistanceM, 15)
	require.InDelta(t, 1500, *got[1].DistanceM, 60)

	got, err = uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng, RadiusM: 2000, Limit: 1})
	require.NoError(t, err)
	require.Len(t, got, 1)

	got, err = uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng, RadiusM: 999999})
	require.NoError(t, err)
	require.Equal(t, []string{"Near Kiosk", "Mid Kiosk", "Far Kiosk"}, names(got), "the radius is capped at 50 km: 10 km is in, 80 km is out")

	got, err = uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng})
	require.NoError(t, err)
	require.Equal(t, []string{"Near Kiosk", "Mid Kiosk"}, names(got), "default radius is 2 km")
}

func TestNearbyOpenNow(t *testing.T) {
	e, uc, t0 := at(t)
	kiosk(e, "open", "Open Kiosk", 100, "OPEN", t0)
	kiosk(e, "shut", "Shut Kiosk", 200, "CLOSED", t0)
	kiosk(e, "paused", "Paused Kiosk", 300, "PAUSED", t0)

	all, err := uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng})
	require.NoError(t, err)
	require.Equal(t, []string{"Open Kiosk", "Shut Kiosk", "Paused Kiosk"}, names(all), "closed outlets are still listed")
	require.True(t, all[0].AcceptingOrders)
	require.False(t, all[1].IsOpenNow)
	require.True(t, all[2].IsOpenNow, "paused stays visible")
	require.False(t, all[2].AcceptingOrders, "but takes no orders")

	open, err := uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng, OpenNow: true})
	require.NoError(t, err)
	require.Equal(t, []string{"Open Kiosk", "Paused Kiosk"}, names(open))
}

func TestNearbyText(t *testing.T) {
	e, uc, t0 := at(t)
	kiosk(e, "a", "Warung Teh", 100, "OPEN", t0)
	kiosk(e, "b", "Kios Roti", 200, "OPEN", t0)

	e.POS.Snapshots["b"] = &entity.POSSnapshot{
		GeneratedAt: t0.Add(time.Minute), Outlet: testutil.POSOutlet("b", "Kios Roti", 1, homeLat, homeLng, "FIXED"),
		Catalog: testutil.Catalog(1, entity.POSProduct{ID: "p1", Name: "Es Cendol", PriceMinor: 1}),
	}
	e.Apply(testutil.Event{ID: "cat", Type: "catalog.changed", Outlet: "b", At: t0.Add(time.Minute), Data: map[string]any{}})

	q := func(text string) []string {
		got, err := uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng, Text: text})
		require.NoError(t, err)

		return names(got)
	}

	require.Equal(t, []string{"Warung Teh"}, q("teh"), "outlet name, any case")
	require.Equal(t, []string{"Kios Roti"}, q("cendol"), "a menu item finds its outlet")
	require.Equal(t, []string{"Warung Teh", "Kios Roti"}, q("warung sri"), "the merchant name matches every outlet of it") //nolint:testifylint
	require.Empty(t, q("sate"))
	require.Empty(t, q("%"), "wildcards in the search are literal")
	require.Empty(t, q("_"))
	require.Len(t, q("   "), 2, "blank text means no filter")
}

func vendorEvents(t0 time.Time, sharing string, fixAge time.Duration) []testutil.Event {
	session := map[string]any{
		"active": true, "sessionId": "s1", "startedAt": t0.UTC().Format(time.RFC3339), "sharing": sharing,
		"spot": map[string]any{"id": "sp", "name": "Pasar", "latitude": homeLat + 0.001, "longitude": homeLng},
	}
	fix := time.Now().Add(-fixAge)

	return []testutil.Event{
		{ID: "ses", Type: "outlet.session.changed", Outlet: "cart", At: t0.Add(time.Minute), Data: session},
		{ID: "fix", Type: "outlet.location.updated", Outlet: "cart", At: t0.Add(2 * time.Minute), Data: map[string]any{
			"mode": "LIVE", "latitude": homeLat + 0.002, "longitude": homeLng, "accuracyM": 6.0, "moving": true,
			"recordedAt": fix.UTC().Format(time.RFC3339Nano), "sessionId": "s1",
		}},
	}
}

func TestNearbyMobileVendor(t *testing.T) {
	cart := testutil.POSOutlet("cart", "Gerobak Bakso", 1, homeLat, homeLng, "MOBILE")

	for _, tc := range []struct {
		name    string
		sharing string
		age     time.Duration
		listed  bool
		mode    string
		stale   bool
		latFrom float64
	}{
		{"fresh live fix", "LIVE", 20 * time.Second, true, "LIVE", false, homeLat + 0.002},
		{"stale fix is flagged", "LIVE", 5 * time.Minute, true, "LIVE", true, homeLat + 0.002},
		{"a fix older than ten minutes is not shown", "LIVE", 11 * time.Minute, false, "", false, 0},
		{"spot sharing never shows the gps", "SPOT", 20 * time.Second, true, "SPOT", false, homeLat + 0.001},
		{"hidden is not shown", "HIDDEN", 20 * time.Second, false, "", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, uc, t0 := at(t)
			e.Apply(testutil.OutletEvent("o", cart, t0))

			// Before the day opens the vendor is not on the map.
			got, err := uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng})
			require.NoError(t, err)
			require.Empty(t, got)

			e.Apply(vendorEvents(t0, tc.sharing, tc.age)...)

			got, err = uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng})
			require.NoError(t, err)

			if !tc.listed {
				require.Empty(t, got)

				return
			}

			require.Len(t, got, 1)
			v := got[0]
			require.Equal(t, tc.mode, v.Location.Mode)
			require.Equal(t, tc.stale, v.Location.Stale)
			require.InDelta(t, tc.latFrom, v.Location.Latitude, 1e-9)
			require.True(t, v.IsOpenNow)
			require.True(t, v.AcceptingOrders)
			require.NotNil(t, v.Today)
			require.Equal(t, "Pasar", *v.Today.SpotName)

			if tc.mode == "SPOT" {
				require.Nil(t, v.Location.RecordedAt, "no trail data leaks through spot sharing")
				require.Nil(t, v.Location.AccuracyM)
				require.False(t, v.Location.Moving)
			}
		})
	}
}

func TestVisibility(t *testing.T) {
	e, uc, t0 := at(t)
	kiosk(e, "k", "Kios", 100, "OPEN", t0)
	id := e.Outlet("k").ID

	o, err := uc.Outlet(e.Ctx, id)
	require.NoError(t, err)
	require.Equal(t, "Kios", o.Name)
	require.Equal(t, "Warung Sri", o.Merchant)

	_, err = uc.Menu(e.Ctx, id)
	require.ErrorIs(t, err, entity.ErrNotFound, "no menu fetched yet")

	e.Apply(testutil.Event{ID: "u", Type: "outlet.unpublished", Outlet: "k", At: t0.Add(time.Minute), Data: map[string]any{}})

	_, err = uc.Outlet(e.Ctx, id)
	require.ErrorIs(t, err, entity.ErrNotFound, "unlisted outlets are not served")

	got, err := uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng})
	require.NoError(t, err)
	require.Empty(t, got)

	// A suspended merchant disappears entirely.
	e.Apply(testutil.OutletEvent("re", testutil.POSOutlet("k", "Kios", 2, homeLat, homeLng, "FIXED"), t0.Add(2*time.Minute)))
	got, err = uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng})
	require.NoError(t, err)
	require.Len(t, got, 1)

	require.NoError(t, e.Store.Suspend(e.Ctx, e.Merchant.ID))

	got, err = uc.Nearby(e.Ctx, discovery.Query{Latitude: homeLat, Longitude: homeLng})
	require.NoError(t, err)
	require.Empty(t, got)

	_, err = uc.Outlet(e.Ctx, id)
	require.ErrorIs(t, err, entity.ErrNotFound)
}

func TestMenuServedAsPublished(t *testing.T) {
	e, uc, t0 := at(t)
	kiosk(e, "k", "Kios", 100, "OPEN", t0)

	e.POS.Snapshots["k"] = &entity.POSSnapshot{
		GeneratedAt: t0.Add(time.Minute), Outlet: testutil.POSOutlet("k", "Kios", 1, homeLat, homeLng, "FIXED"),
		Catalog: testutil.Catalog(7, entity.POSProduct{ID: "p1", Name: "Es Teh", PriceMinor: 500000}),
	}
	e.Apply(testutil.Event{ID: "c", Type: "catalog.changed", Outlet: "k", At: t0.Add(time.Minute), Data: map[string]any{}})

	m, err := uc.Menu(e.Ctx, e.Outlet("k").ID)
	require.NoError(t, err)
	require.Equal(t, 7, m.CatalogVersion)
	require.JSONEq(t, string(testutil.Catalog(7, entity.POSProduct{ID: "p1", Name: "Es Teh", PriceMinor: 500000})), string(m.Menu))
}
