package trust

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
)

const (
	_vendorWindow  = 90 * 24 * time.Hour
	_factorsTTL    = 10 * time.Minute
	_configTTL     = time.Minute
	_sweepBatch    = 200
	_distributionN = 5000
	_timelineMax   = 100
)

// Store is what the service needs from storage.
type Store interface {
	TrustEvents(ctx context.Context, customerID string, since time.Time) ([]entity.TrustEvent, error)
	RecentTrustEvents(ctx context.Context, customerID string, limit int) ([]entity.TrustEvent, error)
	SaveTrustProfile(ctx context.Context, p entity.TrustProfile) error
	TrustConfig(ctx context.Context) (map[string]json.RawMessage, error)
	CustomersToScore(ctx context.Context, limit int) ([]string, error)
	CustomersWithEvents(ctx context.Context, limit int) ([]string, error)
	OpenOrderCount(ctx context.Context, customerID string) (int, error)
	VendorStats(ctx context.Context, since time.Time) ([]entity.VendorStat, error)
}

// Service scores customers. It never refuses anything: in shadow mode it only records what it
// would have done.
type Service struct {
	store Store
	log   logger.Interface
	now   func() time.Time

	labels bool

	mu       sync.Mutex
	cfg      Config
	cfgUntil time.Time

	factors      map[string]float64
	factorsUntil time.Time
}

// Option changes how the service behaves.
type Option func(*Service)

// WithVendorLabels turns on (the default) or off the label vendors are shown with each order.
func WithVendorLabels(on bool) Option { return func(s *Service) { s.labels = on } }

// New builds the service.
func New(store Store, log logger.Interface, opts ...Option) *Service {
	s := &Service{store: store, log: log, now: time.Now, labels: true}
	for _, o := range opts {
		o(s)
	}

	return s
}

// Config returns the rules in force: the defaults, with whatever the trust_config table overrides.
func (s *Service) Config(ctx context.Context) Config {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.now().Before(s.cfgUntil) {
		return s.cfg
	}

	cfg := DefaultConfig()

	rows, err := s.store.TrustConfig(ctx)
	if err != nil {
		// Keep scoring with what we had: a database hiccup must not change the rules.
		s.log.Warn("trust config unreadable, keeping the previous one", "error", err.Error())

		if s.cfg.Weights != nil {
			return s.cfg
		}
	} else {
		cfg = merge(cfg, rows, s.log)
	}

	s.cfg, s.cfgUntil = cfg, s.now().Add(_configTTL)

	return cfg
}

// merge applies the stored rows over the defaults. A row that does not parse is ignored.
func merge(cfg Config, rows map[string]json.RawMessage, log logger.Interface) Config {
	if raw, ok := rows["weights"]; ok {
		var w map[string]Weight
		if err := json.Unmarshal(raw, &w); err != nil {
			log.Warn("trust weights ignored", "error", err.Error())
		} else {
			for k, v := range w {
				cfg.Weights[k] = v
			}
		}
	}

	if raw, ok := rows["settings"]; ok {
		over := cfg
		over.Weights = nil

		if err := json.Unmarshal(raw, &over); err != nil {
			log.Warn("trust settings ignored", "error", err.Error())
		} else {
			over.Weights = cfg.Weights
			cfg = over
		}
	}

	return cfg
}

// Evaluate computes a customer's profile now and stores it.
func (s *Service) Evaluate(ctx context.Context, customerID string) (entity.TrustProfile, error) {
	p, err := s.compute(ctx, customerID)
	if err != nil {
		return entity.TrustProfile{}, err
	}

	if err := s.store.SaveTrustProfile(ctx, p); err != nil {
		return entity.TrustProfile{}, fmt.Errorf("save trust profile: %w", err)
	}

	return p, nil
}

func (s *Service) compute(ctx context.Context, customerID string) (entity.TrustProfile, error) {
	cfg := s.Config(ctx)
	cfg.VendorFactors = s.vendorFactors(ctx, cfg)
	now := s.now()

	events, err := s.store.TrustEvents(ctx, customerID, now.AddDate(0, 0, -cfg.WindowDays))
	if err != nil {
		return entity.TrustProfile{}, err
	}

	r := Score(events, cfg, now)

	return entity.TrustProfile{
		CustomerID: customerID, Score: r.Score, Tier: r.Tier,
		FinishedOrders: r.Finished, CompletedOrders: r.Completed, Reasons: r.Reasons, ComputedAt: now,
	}, nil
}

// Shadow records what the policy would do for a customer who just placed an order. It never
// fails and never changes the answer the customer gets.
func (s *Service) Shadow(ctx context.Context, c entity.Customer, outletID string) {
	p, err := s.Evaluate(ctx, c.ID)
	if err != nil {
		s.log.Warn("trust shadow failed", "customer", c.ID, "error", err.Error())

		return
	}

	open, err := s.store.OpenOrderCount(ctx, c.ID)
	if err != nil {
		open = 0
	}

	policy := PolicyFor(p.Tier)
	diff := Differences(policy, open, c.Phone != nil)

	s.log.Info("trust shadow",
		"customer", c.ID, "outlet", outletID, "tier", p.Tier, "score", p.Score,
		"finished", p.FinishedOrders, "wouldDo", fmt.Sprint(diff))
}

// Sweep scores customers whose events changed since their profile was computed. It reports
// whether more are waiting.
func (s *Service) Sweep(ctx context.Context) (bool, error) {
	ids, err := s.store.CustomersToScore(ctx, _sweepBatch)
	if err != nil {
		return false, err
	}

	for _, id := range ids {
		if _, err := s.Evaluate(ctx, id); err != nil {
			s.log.Warn("trust profile not computed", "customer", id, "error", err.Error())
		}
	}

	return len(ids) == _sweepBatch, nil
}

// Report is what an operator sees about one customer.
type Report struct {
	Profile  entity.TrustProfile `json:"profile"`
	Policy   Policy              `json:"policy"`
	Timeline []entity.TrustEvent `json:"timeline"`
	Config   map[string]any      `json:"config"`
}

// Report computes a customer's profile, stores it, and returns it with the events behind it.
func (s *Service) Report(ctx context.Context, customerID string) (Report, error) {
	p, err := s.Evaluate(ctx, customerID)
	if err != nil {
		return Report{}, err
	}

	timeline, err := s.store.RecentTrustEvents(ctx, customerID, _timelineMax)
	if err != nil {
		return Report{}, err
	}

	if timeline == nil {
		timeline = []entity.TrustEvent{}
	}

	cfg := s.Config(ctx)

	return Report{
		Profile: p, Policy: PolicyFor(p.Tier), Timeline: timeline,
		Config: map[string]any{
			"watchAt": cfg.WatchAt, "restrictedAt": cfg.RestrictedAt, "newBelow": cfg.NewBelow,
			"priorOrders": cfg.PriorOrders, "vendorCap": cfg.VendorCap,
		},
	}, nil
}

// Distribution is how customers spread over the tiers, for tuning before anything is enforced.
type Distribution struct {
	Customers int            `json:"customers"`
	Tiers     map[string]int `json:"tiers"`
	// Percentiles of the score among customers who are not NEW.
	ScoreP50 float64 `json:"scoreP50"`
	ScoreP90 float64 `json:"scoreP90"`
	ScoreP99 float64 `json:"scoreP99"`
	// WouldChange counts customers a tier would limit compared with today.
	WouldLimit int  `json:"wouldLimit"`
	Truncated  bool `json:"truncated"`
}

// Distribution scores every customer with events, without storing anything.
func (s *Service) Distribution(ctx context.Context) (Distribution, error) {
	ids, err := s.store.CustomersWithEvents(ctx, _distributionN+1)
	if err != nil {
		return Distribution{}, err
	}

	d := Distribution{Tiers: map[string]int{}}

	if len(ids) > _distributionN {
		ids, d.Truncated = ids[:_distributionN], true
	}

	var scores []float64

	for _, id := range ids {
		p, err := s.compute(ctx, id)
		if err != nil {
			return Distribution{}, err
		}

		d.Customers++
		d.Tiers[p.Tier]++

		if p.Tier == entity.TierWatch || p.Tier == entity.TierRestricted {
			d.WouldLimit++
		}

		if p.Tier != entity.TierNew {
			scores = append(scores, p.Score)
		}
	}

	sort.Float64s(scores)

	d.ScoreP50, d.ScoreP90, d.ScoreP99 = percentile(scores, 0.50), percentile(scores, 0.90), percentile(scores, 0.99)

	return d, nil
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}

	return sorted[int(math.Min(float64(len(sorted)-1), math.Floor(p*float64(len(sorted)))))]
}

// vendorFactors says how much each merchant's reports about customers count. Vendors that stand
// out from their peers are believed less. It is worked out at most every ten minutes, and a
// failure leaves everybody at full weight rather than failing a score.
func (s *Service) vendorFactors(ctx context.Context, cfg Config) map[string]float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.now().Before(s.factorsUntil) {
		return s.factors
	}

	stats, err := s.store.VendorStats(ctx, s.now().Add(-_vendorWindow))
	if err != nil {
		s.log.Warn("vendor numbers unreadable, trusting every vendor equally", "error", err.Error())

		return nil
	}

	Assess(stats, cfg)
	s.factors, s.factorsUntil = Factors(stats, cfg), s.now().Add(_factorsTTL)

	return s.factors
}

// VendorReport is how vendors compare, for the operator.
type VendorReport struct {
	Peers   Peers               `json:"peers"`
	Vendors []entity.VendorStat `json:"vendors"`
}

// Vendors shows each merchant's numbers next to its peers' and which ones stand out.
func (s *Service) Vendors(ctx context.Context) (VendorReport, error) {
	cfg := s.Config(ctx)

	stats, err := s.store.VendorStats(ctx, s.now().Add(-_vendorWindow))
	if err != nil {
		return VendorReport{}, err
	}

	if stats == nil {
		stats = []entity.VendorStat{}
	}

	return VendorReport{Peers: Assess(stats, cfg), Vendors: stats}, nil
}

// VendorView is what a vendor is shown about a customer with their order: a label and a few
// facts. It is nil when labels are off or the customer cannot be read, so an order is never
// held up by it.
func (s *Service) VendorView(ctx context.Context, customerID string) *entity.VendorTrust {
	if !s.labels {
		return nil
	}

	cfg := s.Config(ctx)
	cfg.VendorFactors = s.vendorFactors(ctx, cfg)
	now := s.now()

	events, err := s.store.TrustEvents(ctx, customerID, now.AddDate(0, 0, -cfg.WindowDays))
	if err != nil {
		s.log.Warn("trust label unreadable", "customer", customerID, "error", err.Error())

		return nil
	}

	r := Score(events, cfg, now)
	recent := now.AddDate(0, 0, -90)
	noShows := 0

	for _, ev := range events {
		if ev.Type == entity.EventNoShow && !ev.CreatedAt.Before(recent) {
			noShows++
		}
	}

	return &entity.VendorTrust{Label: entity.LabelFor(r.Tier), CompletedOrders: r.Completed, NoShowsLast90Days: noShows}
}
