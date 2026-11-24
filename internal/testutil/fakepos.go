package testutil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/pos"
)

// FakePOS answers like a merchant's POS and records what was asked.
type FakePOS struct {
	mu        sync.Mutex
	Outlets_  []entity.POSOutlet
	At        time.Time
	Snapshots map[string]*entity.POSSnapshot
	Err       error
	Calls     []string
}

// Outlets implements the POS reader.
func (f *FakePOS) Outlets(_ context.Context, cred pos.Credentials) ([]entity.POSOutlet, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.Calls = append(f.Calls, "outlets:"+cred.KeyID)

	if f.Err != nil {
		return nil, time.Time{}, f.Err
	}

	return f.Outlets_, f.At, nil
}

// Snapshot implements the POS reader.
func (f *FakePOS) Snapshot(_ context.Context, _ pos.Credentials, id string) (*entity.POSSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.Calls = append(f.Calls, "snapshot:"+id)

	if f.Err != nil {
		return nil, f.Err
	}

	snap, ok := f.Snapshots[id]
	if !ok {
		return nil, errors.New("no snapshot")
	}

	return snap, nil
}

// Verify implements the credential check used when registering a merchant.
func (f *FakePOS) Verify(ctx context.Context, cred pos.Credentials) error {
	_, _, err := f.Outlets(ctx, cred)

	return err
}

// CallCount reports how many calls started with prefix.
func (f *FakePOS) CallCount(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0

	for _, c := range f.Calls {
		if len(c) >= len(prefix) && c[:len(prefix)] == prefix {
			n++
		}
	}

	return n
}

// Catalog builds a raw catalog like the POS serves: one category with the given products.
func Catalog(version int, products ...entity.POSProduct) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"catalogVersion": version,
		"pricing":        map[string]any{"taxRate": 11, "serviceChargeRate": 0},
		"categories": []map[string]any{{
			"id": "cat1", "name": "Minuman", "products": products,
		}},
	})
	if err != nil {
		panic(fmt.Sprintf("catalog: %v", err))
	}

	return raw
}
