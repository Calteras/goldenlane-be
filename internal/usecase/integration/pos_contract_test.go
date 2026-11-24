package integration_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/testutil"
)

// The files in testdata/pos are what the real POS (ansilum-be, src/domain/streetfood)
// produced in a scripted day: events in the order it created them, and its outlet list
// and snapshot. Replaying them proves the two services agree on the wire format. When the
// POS changes its payloads, capture them again and this test shows what broke.
func fixture[T any](t *testing.T, name string) T {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "testutil", "testdata", "pos", name+".json"))
	require.NoError(t, err)

	var v T
	require.NoError(t, json.Unmarshal(raw, &v))

	return v
}

type envelope[T any] struct {
	Data T `json:"data"`
}

func TestReplayRealPOSEvents(t *testing.T) {
	ids := fixture[map[string]string](t, "ids")
	events := fixture[[]json.RawMessage](t, "events")
	snapshot := fixture[envelope[entity.POSSnapshot]](t, "snapshot").Data
	cart, kiosk := ids["cart"], ids["kiosk"]

	e := testutil.NewEnv(t)
	require.Len(t, events, 13)

	// Serve the catalog the POS served. The captured snapshot was taken at the end of
	// the day (it shows an open cart), so for the replay it is restamped to the start of
	// the day, as the POS would have answered then.
	var first struct {
		OccurredAt time.Time `json:"occurredAt"`
	}
	require.NoError(t, json.Unmarshal(events[0], &first))

	atStart := func(o entity.POSOutlet) *entity.POSSnapshot {
		s := snapshot
		s.GeneratedAt = first.OccurredAt
		s.Outlet = o
		s.Outlet.Session, s.Outlet.Location = nil, nil

		return &s
	}
	e.POS.Snapshots[cart] = atStart(e.POSKiosk(t, events, cart))
	e.POS.Snapshots[kiosk] = atStart(e.POSKiosk(t, events, kiosk))

	typeAt := func(i int) string {
		var w struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(events[i], &w))

		return w.Type
	}

	// Replay one event at a time and look at the state after the interesting ones.
	for i, raw := range events {
		require.NoError(t, e.SendRaw(raw), "event %d %s", i, typeAt(i))
		e.Drain()

		switch i {
		case 3: // both outlets announced with their menus
			k := e.Outlet(kiosk)
			require.True(t, k.Published)
			require.Equal(t, "Kios Melati", k.Name)
			require.Len(t, k.Hours, 2, "the overnight Friday interval survives the round trip")
			require.Equal(t, entity.ModeFixed, k.Pos.Mode)
			require.InDelta(t, -6.25, k.Pos.Latitude, 1e-9)

			c := e.Outlet(cart)
			require.Equal(t, entity.ModeMobile, c.LocationMode)
			require.Nil(t, c.Pos, "no open day yet")

			menu, err := e.Store.PublicMenu(e.Ctx, c.ID)
			require.NoError(t, err)
			require.Contains(t, string(menu.Payload), "Es Teh Manis")
			require.Contains(t, string(menu.Payload), "Large", "variants are kept as the POS sent them")
			require.Contains(t, string(menu.Payload), "Gula", "and options")
		case 4: // day opened at the saved spot
			c := e.Outlet(cart)
			require.True(t, c.Session.Active)
			require.Equal(t, "Pasar Minggu", c.Session.Spot.Name)
			require.NotNil(t, c.Pos)
		case 7: // first GPS fix
			p := e.Outlet(cart).Pos
			require.Equal(t, entity.SharingLive, p.Mode)
			require.InDelta(t, -6.2003, p.Latitude, 1e-9)
			require.True(t, p.Moving)
		}
	}

	// After the whole day: the kiosk was unpublished, the day ended, the link revoked.
	require.False(t, e.Outlet(kiosk).Published)
	require.False(t, e.Outlet(cart).Session.Active)
	require.Nil(t, e.Outlet(cart).Pos)
	require.False(t, e.Outlet(cart).Published, "link.revoked hides the merchant")

	var notDone int
	require.NoError(t, e.Pool.QueryRow(e.Ctx, `SELECT count(*) FROM integration_inbox WHERE status <> 'DONE'`).Scan(&notDone))
	require.Zero(t, notDone, "every real event was understood and applied")
}

func TestRealPOSOutletListAndSnapshot(t *testing.T) {
	ids := fixture[map[string]string](t, "ids")
	list := fixture[envelope[struct {
		GeneratedAt string             `json:"generatedAt"`
		Outlets     []entity.POSOutlet `json:"outlets"`
	}]](t, "outlets").Data
	snapshot := fixture[envelope[entity.POSSnapshot]](t, "snapshot").Data

	require.NotEmpty(t, list.GeneratedAt, "the POS stamps its answers")
	require.False(t, snapshot.GeneratedAt.IsZero())
	require.Len(t, list.Outlets, 2, "the day the list was captured, both outlets were published")

	e := testutil.NewEnv(t)
	e.POS.Outlets_, e.POS.At = list.Outlets, snapshot.GeneratedAt

	for _, o := range list.Outlets {
		s := snapshot
		s.Outlet = o
		e.POS.Snapshots[o.OutletID] = &s
	}

	link, err := e.Store.LinkByMerchant(e.Ctx, e.Merchant.ID)
	require.NoError(t, err)
	require.NoError(t, e.Integration.ReconcileLink(e.Ctx, link))

	c := e.Outlet(ids["cart"])
	require.True(t, c.Published)
	require.True(t, c.Session.Active)
	require.Equal(t, "Pasar Minggu", c.Session.Spot.Name)
	require.Equal(t, entity.SharingLive, c.Pos.Mode)
	require.InDelta(t, -6.2004, c.Pos.Latitude, 1e-9)
	require.True(t, c.AcceptingOrders(c.Session.StartedAt.Add(1)), "an open mobile day with a forced-open status takes orders")

	k := e.Outlet(ids["kiosk"])
	require.Equal(t, entity.ModeFixed, k.Pos.Mode)
	require.Len(t, k.Hours, 2)
}
