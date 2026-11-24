package entity_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

func TestFees(t *testing.T) {
	rates := entity.FeeRates{CustomerBps: 250, MerchantBps: 250}

	for name, tc := range map[string]struct {
		total                    int64
		charge, customer, seller int64
	}{
		"whole rupiah fee":     {1000000, 1025000, 25000, 25000}, // Rp 10.000 -> pays Rp 10.250
		"fee rounds up":        {1554000, 1592900, 38900, 38900}, // Rp 15.540 -> 2.5% is Rp 388,50 -> Rp 389
		"small order":          {100, 200, 100, 0},               // Rp 1: fee rounds up to Rp 1, seller's share rounds to nothing
		"price in part-rupiah": {1000050, 1025100, 25050, 25000}, // the charge stays a whole rupiah
	} {
		t.Run(name, func(t *testing.T) {
			f := rates.Compute(tc.total)
			require.Equal(t, tc.charge, f.ChargeMinor)
			require.Equal(t, tc.customer, f.CustomerFeeMinor)
			require.Equal(t, tc.seller, f.MerchantFeeMinor)
			require.Zero(t, f.ChargeMinor%100, "gateways take whole rupiah")
			require.Equal(t, tc.total+f.CustomerFeeMinor, f.ChargeMinor, "the fee is exactly what is charged above the order")
		})
	}

	none := entity.FeeRates{}.Compute(1554000)
	require.EqualValues(t, 1554000, none.ChargeMinor)
	require.Zero(t, none.CustomerFeeMinor)
	require.Zero(t, none.MerchantFeeMinor)
}
