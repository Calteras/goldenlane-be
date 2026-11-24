package integration_test

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/testutil"
)

var ptr = testutil.Ptr[float64]

func TestReceive(t *testing.T) {
	e := testutil.NewEnv(t)
	ev := testutil.Event{ID: "e1", Type: "outlet.updated", Outlet: "o1", Version: 1, Data: map[string]any{"outlet": testutil.POSOutlet("o1", "Kios", 1, -6.2, 106.8, "FIXED")}}

	require.NoError(t, e.Send(ev))
	require.NoError(t, e.Send(ev), "redelivery is accepted")

	var n int
	require.NoError(t, e.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM integration_inbox`).Scan(&n))
	require.Equal(t, 1, n, "a redelivered event is stored once")

	t.Run("wrong secret", func(t *testing.T) {
		require.ErrorIs(t, e.SendAs(e.KeyID, "sfs_other", ev), entity.ErrUnauthorized)
	})
	t.Run("unknown key", func(t *testing.T) {
		require.ErrorIs(t, e.SendAs("sfk_ffffffffffffffffffffffff", testutil.SecretText, ev), entity.ErrUnauthorized)
	})
	t.Run("empty batch", func(t *testing.T) {
		require.ErrorIs(t, e.Send(), entity.ErrBadRequest)
	})
	t.Run("event without an id", func(t *testing.T) {
		require.ErrorIs(t, e.Send(testutil.Event{Type: "x"}), entity.ErrBadRequest)
	})
	t.Run("too many events", func(t *testing.T) {
		many := make([]testutil.Event, 101)
		for i := range many {
			many[i] = testutil.Event{ID: string(rune('a'+i%26)) + string(rune('A'+i/26)), Type: "noop"}
		}

		require.ErrorIs(t, e.Send(many...), entity.ErrBadRequest)
	})
	t.Run("suspended merchant", func(t *testing.T) {
		require.NoError(t, e.Store.Suspend(e.Ctx, e.Merchant.ID))
		require.ErrorIs(t, e.Send(testutil.Event{ID: "late", Type: "noop"}), entity.ErrUnauthorized)
	})
}

func TestOutletLifecycle(t *testing.T) {
	e := testutil.NewEnv(t)
	t0 := time.Now().Add(-time.Hour)

	o := testutil.POSOutlet("o1", "Kios Melati", 1, -6.2, 106.8, "FIXED")
	o.Hours = []entity.Hours{{DayOfWeek: 1, OpensAt: "08:00", ClosesAt: "17:00"}}
	e.Apply(testutil.OutletEvent("e1", o, t0))

	got := e.Outlet("o1")
	require.True(t, got.Published)
	require.Equal(t, "Kios Melati", got.Name)
	require.Equal(t, 1, got.StateVersion)
	require.Len(t, got.Hours, 1)
	require.NotNil(t, got.Pos)
	require.Equal(t, entity.ModeFixed, got.Pos.Mode)

	// A newer version replaces the state; an older one is ignored.
	o2 := testutil.POSOutlet("o1", "Kios Melati Baru", 3, -6.21, 106.81, "FIXED")
	e.Apply(testutil.OutletEvent("e2", o2, t0.Add(time.Minute)))
	require.Equal(t, "Kios Melati Baru", e.Outlet("o1").Name)

	stale := testutil.POSOutlet("o1", "Old name", 2, -6.5, 106.5, "FIXED")
	e.Apply(testutil.OutletEvent("e3", stale, t0.Add(2*time.Minute)))
	got = e.Outlet("o1")
	require.Equal(t, "Kios Melati Baru", got.Name, "an older version never wins")
	require.Equal(t, 3, got.StateVersion)
	require.Empty(t, got.Hours, "hours follow the newest state")

	// Unpublishing hides the outlet; a later publication shows it again.
	e.Apply(testutil.Event{ID: "e4", Type: "outlet.unpublished", Outlet: "o1", At: t0.Add(3 * time.Minute), Data: map[string]any{}})
	require.False(t, e.Outlet("o1").Published)

	o4 := testutil.POSOutlet("o1", "Kios Melati Baru", 4, -6.21, 106.81, "FIXED")
	e.Apply(testutil.OutletEvent("e5", o4, t0.Add(4*time.Minute)))
	require.True(t, e.Outlet("o1").Published)

	// A late unpublish from before the republication must not hide it.
	e.Apply(testutil.Event{ID: "e6", Type: "outlet.unpublished", Outlet: "o1", At: t0.Add(30 * time.Second), Data: map[string]any{}})
	require.True(t, e.Outlet("o1").Published, "ordered by event time, not arrival")
}

func session(active bool, sharing string, spotLat, spotLng float64) map[string]any {
	return map[string]any{
		"active": active, "sessionId": "s1", "startedAt": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"sharing": sharing, "spot": map[string]any{"id": "sp1", "name": "Pasar Minggu", "latitude": spotLat, "longitude": spotLng},
	}
}

func location(lat, lng float64, at time.Time) map[string]any {
	return map[string]any{
		"mode": "LIVE", "latitude": lat, "longitude": lng, "accuracyM": 8.0, "moving": true,
		"recordedAt": at.UTC().Format(time.RFC3339Nano), "sessionId": "s1",
	}
}

func TestMobileVendorPosition(t *testing.T) {
	e := testutil.NewEnv(t)
	t0 := time.Now().Add(-10 * time.Minute)

	cart := testutil.POSOutlet("c1", "Gerobak", 1, -6.2, 106.8, "MOBILE")
	e.Apply(testutil.OutletEvent("o", cart, t0))
	require.Nil(t, e.Outlet("c1").Pos, "a mobile vendor with no open day is not on the map")

	e.Apply(testutil.Event{ID: "s", Type: "outlet.session.changed", Outlet: "c1", At: t0.Add(time.Minute), Data: session(true, "LIVE", -6.2, 106.8)})
	p := e.Outlet("c1").Pos
	require.NotNil(t, p)
	require.Equal(t, entity.SharingSpot, p.Mode, "live, but no fix yet: the spot")

	e.Apply(testutil.Event{ID: "l1", Type: "outlet.location.updated", Outlet: "c1", At: t0.Add(2 * time.Minute), Data: location(-6.205, 106.805, t0.Add(2*time.Minute))})
	p = e.Outlet("c1").Pos
	require.Equal(t, entity.SharingLive, p.Mode)
	require.InDelta(t, -6.205, p.Latitude, 1e-9)

	// A position older than the one we hold is ignored.
	e.Apply(testutil.Event{ID: "l0", Type: "outlet.location.updated", Outlet: "c1", At: t0.Add(90 * time.Second), Data: location(-6.9, 106.9, t0.Add(90*time.Second))})
	require.InDelta(t, -6.205, e.Outlet("c1").Pos.Latitude, 1e-9)

	// Spot-only sharing shows the place, never the GPS.
	e.Apply(testutil.Event{ID: "s2", Type: "outlet.session.changed", Outlet: "c1", At: t0.Add(3 * time.Minute), Data: session(true, "SPOT", -6.2, 106.8)})
	p = e.Outlet("c1").Pos
	require.Equal(t, entity.SharingSpot, p.Mode)
	require.InDelta(t, -6.2, p.Latitude, 1e-9)

	// Hidden shows nothing.
	e.Apply(testutil.Event{ID: "s3", Type: "outlet.session.changed", Outlet: "c1", At: t0.Add(4 * time.Minute), Data: session(true, "HIDDEN", -6.2, 106.8)})
	require.Nil(t, e.Outlet("c1").Pos)

	// Ending the day removes the vendor, and a straggling position cannot bring it back.
	e.Apply(testutil.Event{ID: "s4", Type: "outlet.session.changed", Outlet: "c1", At: t0.Add(5 * time.Minute), Data: session(false, "LIVE", -6.2, 106.8)})
	e.Apply(testutil.Event{ID: "x1", Type: "outlet.location.cleared", Outlet: "c1", At: t0.Add(5 * time.Minute), Data: map[string]any{"reason": "SESSION_ENDED"}})
	e.Apply(testutil.Event{ID: "l9", Type: "outlet.location.updated", Outlet: "c1", At: t0.Add(4 * time.Minute), Data: location(-6.3, 106.3, t0.Add(4*time.Minute))})
	require.Nil(t, e.Outlet("c1").Pos)
	require.False(t, e.Outlet("c1").Session.Active)
}

// The POS delivers at least once and in no guaranteed order. Whatever the order, the
// outlet must end up in the same state.
func TestEventOrderDoesNotMatter(t *testing.T) {
	t0 := time.Now().Add(-10 * time.Minute)

	events := func() []testutil.Event {
		return []testutil.Event{
			testutil.OutletEvent("o1", testutil.POSOutlet("c1", "Gerobak", 1, -6.2, 106.8, "MOBILE"), t0),
			testutil.OutletEvent("o2", testutil.POSOutlet("c1", "Gerobak Baru", 2, -6.2, 106.8, "MOBILE"), t0.Add(time.Minute)),
			{ID: "s1", Type: "outlet.session.changed", Outlet: "c1", At: t0.Add(2 * time.Minute), Data: session(true, "LIVE", -6.2, 106.8)},
			{ID: "l1", Type: "outlet.location.updated", Outlet: "c1", At: t0.Add(3 * time.Minute), Data: location(-6.21, 106.81, t0.Add(3*time.Minute))},
			{ID: "l2", Type: "outlet.location.updated", Outlet: "c1", At: t0.Add(4 * time.Minute), Data: location(-6.22, 106.82, t0.Add(4*time.Minute))},
		}
	}

	var want *entity.Position

	for seed := int64(1); seed <= 8; seed++ {
		e := testutil.NewEnv(t)
		list := events()
		rand.New(rand.NewSource(seed)).Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] }) //nolint:gosec // test shuffle

		// Events whose outlet is not known yet are retried, as in production.
		require.NoError(t, e.Send(list...))

		for round := 0; round < 6; round++ {
			e.Drain()
			_, err := e.Pool.Exec(e.Ctx, `UPDATE integration_inbox SET available_at = now() WHERE status = 'PENDING'`)
			require.NoError(t, err)
		}

		o := e.Outlet("c1")
		require.Equal(t, "Gerobak Baru", o.Name, "seed %d", seed)
		require.NotNil(t, o.Pos, "seed %d", seed)

		if want == nil {
			want = o.Pos
		}

		require.Equal(t, want.Mode, o.Pos.Mode, "seed %d", seed)
		require.InDelta(t, want.Latitude, o.Pos.Latitude, 1e-9, "seed %d", seed)
		require.InDelta(t, -6.22, o.Pos.Latitude, 1e-9, "seed %d: newest position wins", seed)
	}
}

func TestCatalog(t *testing.T) {
	e := testutil.NewEnv(t)
	t0 := time.Now().Add(-time.Hour)
	e.Apply(testutil.OutletEvent("o1", testutil.POSOutlet("o1", "Kios", 1, -6.2, 106.8, "FIXED"), t0))

	e.POS.Snapshots["o1"] = &entity.POSSnapshot{
		GeneratedAt: t0.Add(time.Minute),
		Outlet:      testutil.POSOutlet("o1", "Kios", 1, -6.2, 106.8, "FIXED"),
		Catalog: testutil.Catalog(5,
			entity.POSProduct{ID: "p1", Name: "Es Teh", PriceMinor: 500000},
			entity.POSProduct{ID: "p2", Name: "Roti Bakar", PriceMinor: 1200000, SoldOut: true}),
	}

	e.Apply(testutil.Event{ID: "c1", Type: "catalog.changed", Outlet: "o1", Version: 5, At: t0.Add(time.Minute), Data: map[string]any{}})
	require.Equal(t, 1, e.POS.CallCount("snapshot:o1"))

	menu, err := e.Store.PublicMenu(e.Ctx, e.Outlet("o1").ID)
	require.NoError(t, err)
	require.Equal(t, 5, menu.CatalogVersion)
	require.Contains(t, string(menu.Payload), "Es Teh")

	var n int
	require.NoError(t, e.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM menu_items WHERE sold_out`).Scan(&n))
	require.Equal(t, 1, n)

	// An older catalog never replaces a newer one.
	e.POS.Snapshots["o1"].Catalog = testutil.Catalog(3, entity.POSProduct{ID: "p9", Name: "Old", PriceMinor: 1})
	e.Apply(testutil.Event{ID: "c2", Type: "catalog.changed", Outlet: "o1", Version: 3, At: t0.Add(2 * time.Minute), Data: map[string]any{}})

	menu, err = e.Store.PublicMenu(e.Ctx, e.Outlet("o1").ID)
	require.NoError(t, err)
	require.Equal(t, 5, menu.CatalogVersion)
	require.NotContains(t, string(menu.Payload), "Old")

	// A POS that is down: the event is retried, not lost.
	e.POS.Err = entity.ErrNotFound
	e.Apply(testutil.Event{ID: "c3", Type: "catalog.changed", Outlet: "o1", Version: 6, At: t0.Add(3 * time.Minute), Data: map[string]any{}})

	var status string
	require.NoError(t, e.Pool.QueryRow(e.Ctx, `SELECT status FROM integration_inbox WHERE event_id = 'c3'`).Scan(&status))
	require.Equal(t, "PENDING", status)
}

func TestReconcile(t *testing.T) {
	e := testutil.NewEnv(t)
	now := time.Now()

	kios := testutil.POSOutlet("o1", "Kios", 4, -6.2, 106.8, "FIXED")
	cart := testutil.POSOutlet("c1", "Gerobak", 2, -6.3, 106.9, "MOBILE")
	cart.Session = &entity.POSSession{Active: true, SessionID: "s1", StartedAt: &now, Sharing: "LIVE", Spot: &entity.Spot{Name: "Gerbang", Latitude: ptr(-6.3), Longitude: ptr(106.9)}}
	cart.Location = &entity.POSLocation{Mode: "LIVE", Latitude: -6.31, Longitude: 106.91, RecordedAt: &now}

	e.POS.Outlets_, e.POS.At = []entity.POSOutlet{kios, cart}, now
	for _, o := range e.POS.Outlets_ {
		e.POS.Snapshots[o.OutletID] = &entity.POSSnapshot{GeneratedAt: now, Outlet: o, Catalog: testutil.Catalog(1, entity.POSProduct{ID: "p", Name: "Es Teh", PriceMinor: 1})}
	}

	link, err := e.Store.LinkByMerchant(e.Ctx, e.Merchant.ID)
	require.NoError(t, err)
	require.NoError(t, e.Integration.ReconcileLink(e.Ctx, link))

	require.True(t, e.Outlet("o1").Published)

	c := e.Outlet("c1")
	require.True(t, c.Published)
	require.True(t, c.Session.Active)
	require.Equal(t, entity.SharingLive, c.Pos.Mode)
	require.InDelta(t, -6.31, c.Pos.Latitude, 1e-9)
	require.Equal(t, 2, e.POS.CallCount("snapshot:"), "menus are fetched for new outlets")

	// A second pass does not refetch fresh menus.
	require.NoError(t, e.Integration.ReconcileLink(e.Ctx, link))
	require.Equal(t, 2, e.POS.CallCount("snapshot:"))

	// The vendor went home and the kiosk was unpublished while we were not listening.
	later := now.Add(time.Minute)
	cart.StateVersion, cart.Session, cart.Location = 3, nil, nil
	e.POS.Outlets_, e.POS.At = []entity.POSOutlet{cart}, later
	require.NoError(t, e.Integration.ReconcileLink(e.Ctx, link))

	require.False(t, e.Outlet("o1").Published, "no longer listed by the POS")
	c = e.Outlet("c1")
	require.False(t, c.Session.Active)
	require.Nil(t, c.Pos)

	// A POS that cannot be reached changes nothing.
	e.POS.Err = entity.ErrNotFound
	require.Error(t, e.Integration.ReconcileLink(e.Ctx, link))
	require.True(t, e.Outlet("c1").Published)
}

func TestLinkRevoked(t *testing.T) {
	e := testutil.NewEnv(t)
	e.Apply(testutil.OutletEvent("o1", testutil.POSOutlet("o1", "Kios", 1, -6.2, 106.8, "FIXED"), time.Now().Add(-time.Minute)))
	require.True(t, e.Outlet("o1").Published)

	e.Apply(testutil.Event{ID: "r", Type: "link.revoked", Data: map[string]any{}})
	require.False(t, e.Outlet("o1").Published)
	require.ErrorIs(t, e.Send(testutil.Event{ID: "after", Type: "noop"}), entity.ErrUnauthorized, "a revoked link is closed")
}

func TestUnknownEventTypesAreIgnored(t *testing.T) {
	e := testutil.NewEnv(t)
	e.Apply(testutil.Event{ID: "x", Type: "something.new", Data: map[string]any{"a": 1}})
	e.Apply(testutil.Event{ID: "y", Type: "order.status.changed", Outlet: "o1", Data: map[string]any{}})

	var failed int
	require.NoError(t, e.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM integration_inbox WHERE status <> 'DONE'`).Scan(&failed))
	require.Zero(t, failed)
}

// A day opened at a raw position (no saved spot) has an anchor but no spot. Spot-only
// sharing must still show that place. (Found by running against the real POS.)
func TestDayHeldAtAnUnsavedPlace(t *testing.T) {
	t0 := time.Now().Add(-10 * time.Minute)
	unnamed := func(sharing string) map[string]any {
		return map[string]any{
			"active": true, "sessionId": "s1", "startedAt": t0.UTC().Format(time.RFC3339), "sharing": sharing,
			"spot": nil, "anchor": map[string]any{"latitude": -6.2001, "longitude": 106.8001},
		}
	}

	t.Run("from events", func(t *testing.T) {
		e := testutil.NewEnv(t)
		e.Apply(testutil.OutletEvent("o", testutil.POSOutlet("c1", "Gerobak", 1, -6.2, 106.8, "MOBILE"), t0))
		e.Apply(testutil.Event{ID: "s", Type: "outlet.session.changed", Outlet: "c1", At: t0.Add(time.Minute), Data: unnamed("SPOT")})

		p := e.Outlet("c1").Pos
		require.NotNil(t, p, "the vendor is shown")
		require.Equal(t, entity.SharingSpot, p.Mode)
		require.InDelta(t, -6.2001, p.Latitude, 1e-9)
		require.Empty(t, e.Outlet("c1").Session.Spot.Name, "an unnamed place has no name to show")
	})

	t.Run("from a reconciliation", func(t *testing.T) {
		e := testutil.NewEnv(t)
		cart := testutil.POSOutlet("c1", "Gerobak", 1, -6.2, 106.8, "MOBILE")
		cart.Session = &entity.POSSession{Active: true, SessionID: "s1", StartedAt: &t0, Sharing: "SPOT", Anchor: &entity.Anchor{Latitude: -6.2001, Longitude: 106.8001}}
		e.POS.Outlets_, e.POS.At = []entity.POSOutlet{cart}, time.Now()
		e.POS.Snapshots["c1"] = &entity.POSSnapshot{GeneratedAt: time.Now(), Outlet: cart, Catalog: testutil.Catalog(1)}

		link, err := e.Store.LinkByMerchant(e.Ctx, e.Merchant.ID)
		require.NoError(t, err)
		require.NoError(t, e.Integration.ReconcileLink(e.Ctx, link))

		p := e.Outlet("c1").Pos
		require.NotNil(t, p)
		require.Equal(t, entity.SharingSpot, p.Mode)
		require.InDelta(t, -6.2001, p.Latitude, 1e-9)
	})
}
