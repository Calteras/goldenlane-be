package testutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)

	return filepath.Join(filepath.Dir(file), "..", "..")
}

// Kiosk is a published outlet with the real menu the POS served in the captured day:
// "Es Teh Manis", with a Large variant and a required "Gula" option group.
type Kiosk struct {
	Outlet   *entity.Outlet
	Product  string
	Variant  string
	Modifier string
}

// PublishKiosk lists a fixed outlet that is open now and loads its menu.
func (e *Env) PublishKiosk(t *testing.T) Kiosk {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(), "internal", "testutil", "testdata", "pos", "snapshot.json"))
	require.NoError(t, err)

	var snap struct {
		Data entity.POSSnapshot `json:"data"`
	}

	require.NoError(t, json.Unmarshal(raw, &snap))

	var catalog struct {
		Categories []struct {
			Products []struct {
				ID       string `json:"id"`
				Variants []struct {
					ID string `json:"id"`
				} `json:"variants"`
				ModifierGroups []struct {
					Options []struct {
						ID string `json:"id"`
					} `json:"options"`
				} `json:"modifierGroups"`
			} `json:"products"`
		} `json:"categories"`
	}

	require.NoError(t, json.Unmarshal(snap.Data.Catalog, &catalog))

	p := catalog.Categories[0].Products[0]
	k := Kiosk{Product: p.ID, Variant: p.Variants[0].ID, Modifier: p.ModifierGroups[0].Options[0].ID}

	at := time.Now().Add(-time.Hour)
	outlet := POSOutlet("kiosk-1", "Kios Melati", 1, -6.25, 106.85, "FIXED")
	e.POS.Snapshots["kiosk-1"] = &entity.POSSnapshot{GeneratedAt: at.Add(time.Minute), Outlet: outlet, Catalog: snap.Data.Catalog}

	e.Apply(
		OutletEvent("kiosk-outlet", outlet, at),
		Event{ID: "kiosk-catalog", Type: "catalog.changed", Outlet: "kiosk-1", At: at.Add(time.Minute), Data: map[string]any{}},
	)

	k.Outlet = e.Outlet("kiosk-1")

	return k
}

// Customer registers an app user.
func (e *Env) Customer(t *testing.T, uid, name, phone string) entity.Customer {
	t.Helper()

	c, err := e.Store.FindOrCreateCustomer(e.Ctx, "firebase", uid, entity.SignInProfile{Phone: phone, Name: name})
	require.NoError(t, err)

	return c
}
