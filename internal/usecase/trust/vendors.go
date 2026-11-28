package trust

import (
	"math"
	"sort"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
)

// Flags a vendor can get for standing out from its peers.
const (
	FlagNoShowRate = "NO_SHOW_RATE" // calls far more of its customers no-shows than the others do
	FlagCancelRate = "CANCEL_RATE"  // cancels far more orders it had accepted
	FlagRejectRate = "REJECT_RATE"  // rejects, or never answers, far more orders
)

// MinPeers is how many comparable vendors it takes to say one stands out: a median of one or
// two vendors says nothing.
const MinPeers = 3

func rate(n, of int) float64 {
	if of == 0 {
		return 0
	}

	return math.Round(float64(n)/float64(of)*1000) / 1000
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}

	return (sorted[mid-1] + sorted[mid]) / 2
}

// Peers are the medians of the vendors that have enough history to compare.
type Peers struct {
	Vendors      int     `json:"vendors"`
	NoShowRate   float64 `json:"noShowRate"`
	CancelRate   float64 `json:"cancelRate"`
	RejectRate   float64 `json:"rejectRate"`
	Comparable   bool    `json:"comparable"`
	MinPeers     int     `json:"minPeers"`
	MinOrders    int     `json:"minOrders"`
	OutlierTimes float64 `json:"outlierMultiple"`
}

// Assess computes each vendor's rates and flags the ones that stand out from their peers. A
// vendor stands out on a measure when it has at least VendorMinEvents of it and its rate is at
// least both the floor and OutlierMultiple times the peer median. With fewer than MinPeers
// comparable vendors nobody is flagged. It changes the stats it is given in place.
func Assess(stats []entity.VendorStat, cfg Config) Peers {
	var noShow, cancel, reject []float64

	for i := range stats {
		s := &stats[i]
		s.DecidedOrders = s.Completed + s.NoShows + s.VendorCancelled + s.VendorRejected + s.VendorNoResponse
		s.NoShowRate = rate(s.NoShows, s.Completed+s.NoShows)
		s.CancelRate = rate(s.VendorCancelled, s.DecidedOrders)
		s.RejectRate = rate(s.VendorRejected+s.VendorNoResponse, s.DecidedOrders)
		s.Flags = []string{}

		if s.DecidedOrders >= cfg.VendorMinOrders {
			noShow = append(noShow, s.NoShowRate)
			cancel = append(cancel, s.CancelRate)
			reject = append(reject, s.RejectRate)
		}
	}

	p := Peers{
		Vendors: len(noShow), NoShowRate: median(noShow), CancelRate: median(cancel), RejectRate: median(reject),
		Comparable: len(noShow) >= MinPeers, MinPeers: MinPeers, MinOrders: cfg.VendorMinOrders, OutlierTimes: cfg.OutlierMultiple,
	}
	if !p.Comparable {
		return p
	}

	stands := func(r, med, floor float64, events int) bool {
		return events >= cfg.VendorMinEvents && r >= floor && r >= cfg.OutlierMultiple*med
	}

	for i := range stats {
		s := &stats[i]
		if s.DecidedOrders < cfg.VendorMinOrders {
			continue
		}

		if stands(s.NoShowRate, p.NoShowRate, cfg.NoShowFloor, s.NoShows) {
			s.Flags = append(s.Flags, FlagNoShowRate)
		}

		if stands(s.CancelRate, p.CancelRate, cfg.CancelFloor, s.VendorCancelled) {
			s.Flags = append(s.Flags, FlagCancelRate)
		}

		if stands(s.RejectRate, p.RejectRate, cfg.CancelFloor, s.VendorRejected+s.VendorNoResponse) {
			s.Flags = append(s.Flags, FlagRejectRate)
		}
	}

	return p
}

// Factors says how much each flagged vendor's reports about customers count. Only a vendor
// that calls too many no-shows is discounted: the others' numbers say something about the
// vendor, not about whether to believe what it says of customers.
func Factors(stats []entity.VendorStat, cfg Config) map[string]float64 {
	out := map[string]float64{}

	for _, s := range stats {
		for _, f := range s.Flags {
			if f == FlagNoShowRate {
				out[s.MerchantID] = cfg.OutlierFactor
			}
		}
	}

	return out
}
