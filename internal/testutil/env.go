package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/persistent"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/integration"
	"github.com/Calteras/streetfood-customer-api/pkg/secretbox"
	"github.com/Calteras/streetfood-customer-api/pkg/signing"
)

// SecretText is the shared secret of the test merchant.
const SecretText = "sfs_test_secret_value"

// SilentLogger discards logs.
type SilentLogger struct{}

func (SilentLogger) Debug(string, ...interface{}) {}
func (SilentLogger) Info(string, ...interface{})  {}
func (SilentLogger) Warn(string, ...interface{})  {}
func (SilentLogger) Error(string, ...interface{}) {}
func (SilentLogger) Fatal(string, ...interface{}) {}

// Event is a POS event to send.
type Event struct {
	ID      string
	Type    string
	Outlet  string
	Version int
	At      time.Time
	Data    any
}

// Env is a database, a registered merchant and a fake POS.
type Env struct {
	T           *testing.T
	Ctx         context.Context
	Pool        *pgxpool.Pool
	Store       *persistent.Store
	Box         *secretbox.Box
	POS         *FakePOS
	Integration *integration.UseCase
	Merchant    entity.Merchant
	KeyID       string
}

// NewEnv prepares everything. The test is skipped without TEST_PG_URL.
func NewEnv(t *testing.T) *Env {
	t.Helper()

	pool := DB(t)
	box, err := secretbox.New(bytes.Repeat([]byte{9}, 32))
	require.NoError(t, err)

	e := &Env{
		T: t, Ctx: context.Background(), Pool: pool, Store: persistent.New(pool), Box: box,
		POS: &FakePOS{Snapshots: map[string]*entity.POSSnapshot{}}, KeyID: "sfk_" + "0123456789abcdef01234567",
	}
	e.Integration = integration.New(e.Store, e.POS, box, SilentLogger{})
	e.Merchant = e.AddMerchant("Warung Sri", "tenant-1", e.KeyID, SecretText)

	return e
}

// AddMerchant registers another merchant with its secret sealed like production does.
func (e *Env) AddMerchant(name, tenant, keyID, secret string) entity.Merchant {
	e.T.Helper()

	id := uuid.NewString()
	sealed, err := e.Box.Seal(secret, id)
	require.NoError(e.T, err)

	m, err := e.Store.CreateMerchant(e.Ctx, id, name, tenant, keyID, sealed, "http://pos.test")
	require.NoError(e.T, err)

	return m
}

// Send signs and delivers a batch the way the POS does.
func (e *Env) Send(events ...Event) error {
	e.T.Helper()

	return e.SendAs(e.KeyID, SecretText, events...)
}

// SendAs signs with explicit credentials, for negative tests.
func (e *Env) SendAs(keyID, secret string, events ...Event) error {
	e.T.Helper()

	wire := make([]map[string]any, 0, len(events))

	for _, ev := range events {
		at := ev.At
		if at.IsZero() {
			at = time.Now()
		}

		var outlet any
		if ev.Outlet != "" {
			outlet = ev.Outlet
		}

		wire = append(wire, map[string]any{
			"eventId": ev.ID, "type": ev.Type, "outletId": outlet, "version": ev.Version,
			"occurredAt": at.UTC().Format(time.RFC3339Nano), "data": ev.Data,
		})
	}

	body, err := json.Marshal(map[string]any{"events": wire})
	require.NoError(e.T, err)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	path := "/v1/integrations/pos/events"

	return e.Integration.Receive(e.Ctx, keyID, ts, signing.Sign(secret, ts, "POST", path, body), "POST", path, body)
}

// Drain applies every stored event.
func (e *Env) Drain() {
	e.T.Helper()

	for i := 0; i < 50; i++ {
		more, err := e.Integration.Process(e.Ctx)
		require.NoError(e.T, err)

		if !more {
			// One more pass picks up events that were retried with no delay.
			break
		}
	}
}

// Apply sends events and applies them.
func (e *Env) Apply(events ...Event) {
	e.T.Helper()
	require.NoError(e.T, e.Send(events...))
	e.Drain()
}

// Outlet loads an outlet by its POS id, listed or not.
func (e *Env) Outlet(posOutletID string) *entity.Outlet {
	e.T.Helper()

	o, err := e.Store.OutletByPOSID(e.Ctx, e.Merchant.ID, posOutletID)
	require.NoError(e.T, err)

	return o
}

// Ptr returns a pointer to v.
func Ptr[T any](v T) *T { return &v }

// POSOutlet builds a POS outlet state.
func POSOutlet(id, name string, version int, lat, lng float64, mode string) entity.POSOutlet {
	return entity.POSOutlet{
		OutletID: id, Active: true, Name: name, Address: Ptr("Jl. Melati 3"),
		Latitude: &lat, Longitude: &lng, LocationMode: mode, LocationSharing: "LIVE",
		Timezone: "Asia/Jakarta", OnlineStatus: "OPEN", PrepTimeMinutes: 10, PricingValid: true,
		StateVersion: version,
	}
}

// OutletEvent wraps a state as an outlet.updated event.
func OutletEvent(id string, o entity.POSOutlet, at time.Time) Event {
	return Event{ID: id, Type: "outlet.updated", Outlet: o.OutletID, Version: o.StateVersion, At: at, Data: map[string]any{"outlet": o}}
}

// SendRaw delivers events exactly as captured, signed like the POS signs them.
func (e *Env) SendRaw(events ...json.RawMessage) error {
	e.T.Helper()

	body, err := json.Marshal(map[string]any{"events": events})
	require.NoError(e.T, err)

	ts := strconv.FormatInt(time.Now().Unix(), 10)
	path := "/v1/integrations/pos/events"

	return e.Integration.Receive(e.Ctx, e.KeyID, ts, signing.Sign(SecretText, ts, "POST", path, body), "POST", path, body)
}

// POSKiosk finds the outlet state a captured outlet.updated event carried for an outlet.
func (e *Env) POSKiosk(t *testing.T, events []json.RawMessage, posOutletID string) entity.POSOutlet {
	t.Helper()

	for _, raw := range events {
		var ev struct {
			Type string `json:"type"`
			Data struct {
				Outlet entity.POSOutlet `json:"outlet"`
			} `json:"data"`
		}

		require.NoError(t, json.Unmarshal(raw, &ev))

		if ev.Type == "outlet.updated" && ev.Data.Outlet.OutletID == posOutletID {
			return ev.Data.Outlet
		}
	}

	t.Fatalf("no outlet.updated for %s", posOutletID)

	return entity.POSOutlet{}
}
