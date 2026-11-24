package entity_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

func at(t *testing.T, iso string) time.Time {
	t.Helper()

	v, err := time.Parse(time.RFC3339, iso)
	require.NoError(t, err)

	return v
}

// The same cases the POS checks in its own hours logic (ansilum-be streetfood-hours.ts):
// 2026-11-18 is a Wednesday and Jakarta is UTC+7.
func TestWithinHours(t *testing.T) {
	day := []entity.Hours{{DayOfWeek: 3, OpensAt: "08:00", ClosesAt: "17:00"}}
	night := []entity.Hours{{DayOfWeek: 3, OpensAt: "18:00", ClosesAt: "02:00"}}
	jkt := "Asia/Jakarta"

	require.True(t, entity.WithinHours(day, at(t, "2026-11-18T02:00:00Z"), jkt), "09:00 Wed")
	require.False(t, entity.WithinHours(day, at(t, "2026-11-18T13:00:00Z"), jkt), "20:00 Wed")
	require.True(t, entity.WithinHours(night, at(t, "2026-11-18T13:00:00Z"), jkt), "20:00 Wed, overnight")
	require.True(t, entity.WithinHours(night, at(t, "2026-11-18T18:00:00Z"), jkt), "01:00 Thu, tail of Wednesday")
	require.False(t, entity.WithinHours(night, at(t, "2026-11-18T20:00:00Z"), jkt), "03:00 Thu, ended")
	require.False(t, entity.WithinHours(nil, at(t, "2026-11-18T02:00:00Z"), jkt))
	// A bad timezone falls back to UTC: 09:00 UTC on Wednesday is inside 08-17.
	require.True(t, entity.WithinHours(day, at(t, "2026-11-18T09:00:00Z"), "Not/AZone"))
	require.False(t, entity.WithinHours(day, at(t, "2026-11-18T02:00:00Z"), "Not/AZone"))
}

func TestOpenStates(t *testing.T) {
	now := at(t, "2026-11-18T13:00:00Z") // 20:00 Wed Jakarta, outside 08-17
	hours := []entity.Hours{{DayOfWeek: 3, OpensAt: "08:00", ClosesAt: "17:00"}}
	fixed := func(status string) *entity.Outlet {
		return &entity.Outlet{
			LocationMode: entity.ModeFixed, Active: true, PricingValid: true,
			OnlineStatus: status, Hours: hours, Timezone: "Asia/Jakarta",
		}
	}

	require.False(t, fixed("AUTO").IsOpenNow(now))
	require.True(t, fixed("OPEN").IsOpenNow(now), "forced open")
	require.False(t, fixed("CLOSED").IsOpenNow(now))
	require.True(t, fixed("PAUSED").IsOpenNow(now), "paused stays visible")
	require.False(t, fixed("PAUSED").AcceptingOrders(now), "but takes no orders")
	require.True(t, fixed("OPEN").AcceptingOrders(now))

	broken := fixed("OPEN")
	broken.PricingValid = false
	require.False(t, broken.AcceptingOrders(now))

	// A mobile vendor is out only while its day is open.
	cart := fixed("OPEN")
	cart.LocationMode = entity.ModeMobile
	require.False(t, cart.IsOpenNow(now))
	require.False(t, cart.AcceptingOrders(now))
	cart.Session.Active = true
	require.True(t, cart.IsOpenNow(now))
	require.True(t, cart.AcceptingOrders(now))
}

func ptr[T any](v T) *T { return &v }

func TestDerivePosition(t *testing.T) {
	spot := &entity.Spot{Name: "Pasar", Latitude: ptr(-6.2), Longitude: ptr(106.8)}
	recorded := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	live := entity.Current{Latitude: ptr(-6.21), Longitude: ptr(106.81), RecordedAt: &recorded, Moving: true}

	kiosk := &entity.Outlet{LocationMode: entity.ModeFixed, BaseLat: ptr(-6.25), BaseLng: ptr(106.85)}
	p := kiosk.DerivePosition()
	require.NotNil(t, p)
	require.Equal(t, entity.ModeFixed, p.Mode)
	require.Nil(t, (&entity.Outlet{LocationMode: entity.ModeFixed}).DerivePosition(), "no coordinates, no position")

	cart := &entity.Outlet{LocationMode: entity.ModeMobile, Current: live, Session: entity.Session{Active: false, Sharing: entity.SharingLive, Spot: spot}}
	require.Nil(t, cart.DerivePosition(), "no open day, not out")

	cart.Session.Active = true
	p = cart.DerivePosition()
	require.Equal(t, entity.SharingLive, p.Mode)
	require.InDelta(t, -6.21, p.Latitude, 1e-9)
	require.True(t, p.Moving)

	cart.Session.Sharing = entity.SharingSpot
	p = cart.DerivePosition()
	require.Equal(t, entity.SharingSpot, p.Mode, "spot sharing never shows the GPS trail")
	require.InDelta(t, -6.2, p.Latitude, 1e-9)

	cart.Session.Sharing = entity.SharingHidden
	require.Nil(t, cart.DerivePosition())

	cart.Session.Sharing = entity.SharingLive
	cart.Current = entity.Current{}
	p = cart.DerivePosition()
	require.Equal(t, entity.SharingSpot, p.Mode, "live but no fix yet falls back to the spot")

	cart.Session.Spot = nil
	require.Nil(t, cart.DerivePosition())
}

func TestFreshness(t *testing.T) {
	now := time.Date(2026, 11, 18, 1, 0, 0, 0, time.UTC)
	pos := func(age time.Duration, mode string) *entity.Position {
		ts := now.Add(-age)
		return &entity.Position{Mode: mode, RecordedAt: &ts}
	}

	require.Equal(t, "fresh", pos(time.Minute, entity.SharingLive).Freshness(now))
	require.Equal(t, "stale", pos(4*time.Minute, entity.SharingLive).Freshness(now))
	require.Equal(t, "hidden", pos(11*time.Minute, entity.SharingLive).Freshness(now))
	require.Equal(t, "fresh", pos(time.Hour, entity.SharingSpot).Freshness(now), "a spot does not go stale")
	require.Equal(t, "fresh", pos(time.Hour, entity.ModeFixed).Freshness(now))
}
