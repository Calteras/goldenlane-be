// Package app wires the service together and runs it.
package app

import (
	"context"
	"os/signal"
	"syscall"
	"time"

	"github.com/Calteras/streetfood-customer-api/config"
	httpctl "github.com/Calteras/streetfood-customer-api/internal/controller/http"
	"github.com/Calteras/streetfood-customer-api/internal/controller/poller"
	"github.com/Calteras/streetfood-customer-api/internal/entity"
	"github.com/Calteras/streetfood-customer-api/internal/repository/persistent"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/fcm"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/firebase"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/gateway"
	"github.com/Calteras/streetfood-customer-api/internal/repository/webapi/pos"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/discovery"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/identity"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/integration"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/merchant"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/ordering"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/payment"
	"github.com/Calteras/streetfood-customer-api/internal/usecase/trust"
	"github.com/Calteras/streetfood-customer-api/pkg/httpserver"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
	"github.com/Calteras/streetfood-customer-api/pkg/postgres"
	"github.com/Calteras/streetfood-customer-api/pkg/secretbox"
	"github.com/Calteras/streetfood-customer-api/pkg/signals"
)

const _shutdownTimeout = 10 * time.Second

// Run builds every dependency, serves until a signal arrives, then shuts down in order.
func Run(cfg *config.Config) {
	// Times are UTC everywhere: in answers (so a "+" never ends up in a query string), in
	// logs, and whatever zone the host runs in.
	time.Local = time.UTC

	l := logger.New(cfg.Log.Level, cfg.App.Name)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Workers sleep until work exists. The tracer hears this process's own commits: a commit that
	// wrote rows wakes every worker, and a committed pg_notify wakes the watchers of an order.
	// Nothing polls the database and no connection is held for LISTEN, so Neon can suspend it.
	//
	// With Redis, the same signals also go to the other instances, and theirs come here.
	var (
		group poller.Group
		hub   = ordering.NewHub()
		bus   *signals.Bus
		cache *discovery.Cached
	)

	if cfg.Redis.URL != "" {
		b, err := signals.New(cfg.Redis.URL, cfg.App.Name+":"+cfg.App.Env+":signals", l)
		if err != nil {
			l.Fatal("app - Run - signals", "error", err.Error())
		}

		bus = b
	}

	// A write that touched what customers browse drops the discovery snapshot, here and elsewhere.
	wrote := func(tables []string) {
		group.Wake()

		if cache != nil && discovery.Watches(tables) {
			cache.Invalidate()
		}
	}

	pg, err := postgres.New(cfg.PG.URL,
		postgres.MaxPoolSize(cfg.PG.PoolMax),
		postgres.ApplicationName(cfg.App.Name),
		postgres.Tracer(&postgres.CommitTracer{
			OnWrite: func(tables []string) {
				wrote(tables)
				bus.Publish(signals.Message{Kind: signals.KindWrite, Tables: tables})
			},
			OnNotify: func(n postgres.Notification) {
				hub.OnNotification(n.Channel, n.Payload)
				bus.Publish(signals.Message{Kind: signals.KindNotify, Channel: n.Channel, Payload: n.Payload})
			},
		}),
	)
	if err != nil {
		l.Fatal("app - Run - postgres.New", "error", err.Error())
	}
	defer pg.Close()

	key, err := cfg.EncryptionKeyBytes()
	if err != nil {
		l.Fatal("app - Run - encryption key", "error", err.Error())
	}

	box, err := secretbox.New(key)
	if err != nil {
		l.Fatal("app - Run - secretbox", "error", err.Error())
	}

	verifier, err := firebase.New(ctx, cfg.Firebase.JWKSURL, cfg.Firebase.ProjectID, cfg.Firebase.Issuer)
	if err != nil {
		l.Fatal("app - Run - firebase", "error", err.Error())
	}

	store := persistent.New(pg.Pool)
	posClient := pos.New()

	integ := integration.New(store, posClient, box, l)
	ident := identity.New(store, verifier)
	ident.OnForget(func(customerID string) {
		bus.Publish(signals.Message{Kind: signals.KindForget, Payload: customerID})
	})

	cache = discovery.NewCached(store)
	disc := discovery.New(cache)
	merch := merchant.New(store, posClient, integ, box, merchant.Settings{
		AllowedHosts: cfg.POSHosts(),
		Production:   cfg.IsProduction(),
	}, l)

	pay := payment.New(store, gateway.NewRegistry(gateway.Endpoints{
		Xendit: cfg.Payments.XenditURL, MidtransSnap: cfg.Payments.MidtransSnapURL, MidtransAPI: cfg.Payments.MidtransAPIURL,
	}), box, payment.Settings{
		Rates:         entity.FeeRates{CustomerBps: cfg.Payments.CustomerFeeBps, MerchantBps: cfg.Payments.MerchantFeeBps},
		Window:        cfg.Payments.Window,
		ReturnURL:     cfg.Payments.ReturnURL,
		PublicBaseURL: cfg.Payments.PublicBaseURL,
	}, l)

	orders := ordering.New(store, posClient, pay, box, l)
	scorer := trust.New(store, l)
	orders.SetTrust(scorer)
	pay.SetOrderNotifier(orders.Notification)
	integ.SetOrderMirror(orders)

	// Push is optional: without a service account the queue is drained unsent.
	var pusher ordering.Pusher

	if cfg.Firebase.ServiceAccountFile != "" {
		client, err := fcm.New(ctx, cfg.Firebase.ServiceAccountFile, cfg.Firebase.ProjectID)
		if err != nil {
			l.Fatal("app - Run - fcm", "error", err.Error())
		}

		pusher = client
	} else {
		l.Warn("push notifications are off (FCM_SERVICE_ACCOUNT_FILE is not set)")
	}

	server := httpserver.New(httpserver.Port(cfg.HTTP.Port))
	httpctl.NewRouter(server.App, cfg, l, httpctl.Dependencies{
		Auth: ident, Events: integ, Discovery: disc, Profiles: ident, Merchants: merch, Orders: orders, Hub: hub, Payments: pay, Trust: scorer,
	})

	notifier := ordering.NewNotifier(store, pusher, l)

	// Workers with a deadline sleep until the next item falls due (a retry, a lease, a payment
	// window). The others only run when woken or on the sweep: the POS pushes order changes and
	// the gateway calls back, so re-reading orders and payments is a safety net.
	sweep := cfg.Workers.SweepInterval
	payDue := func(ctx context.Context) (time.Duration, bool, error) {
		return store.PaymentCheckDue(ctx, payment.PollAfter)
	}

	workers := []*poller.Poller{
		poller.New("pos events", integ.Process, sweep, l, poller.WithDue(store.InboxDue)),
		poller.New("pos reconcile", integ.Reconcile, cfg.Integration.ReconcileInterval, l),
		poller.New("order submit", orders.ProcessSubmits, sweep, l, poller.WithDue(store.SubmitDue)),
		poller.New("order refresh", orders.RefreshStale, sweep, l),
		poller.New("push", notifier.Process, sweep, l, poller.WithDue(store.NotificationDue)),
		poller.New("payment check", pay.Poll, sweep, l, poller.WithDue(payDue)),
		poller.New("payment expiry", pay.ExpireUnpaid, sweep, l, poller.WithDue(store.UnpaidDue)),
		poller.New("refunds", pay.Refund, sweep, l, poller.WithDue(store.RefundDue)),
		// Scores customers whose events changed. A scheduled read: no deadline to wake on.
		poller.New("trust profiles", scorer.Sweep, sweep, l),
	}

	// The reconcile worker does not wake on commits: it is a scheduled read of the POS.
	group.Add(workers[0], workers[2], workers[3], workers[4], workers[5], workers[6], workers[7])

	for _, w := range workers {
		w.Start()
	}

	// Signals from the other instances: the same reactions as to this instance's own commits.
	go bus.Run(ctx, func(m signals.Message) {
		switch m.Kind {
		case signals.KindWrite:
			wrote(m.Tables)
		case signals.KindNotify:
			hub.OnNotification(m.Channel, m.Payload)
		case signals.KindForget:
			ident.Forget(m.Payload)
		}
	})

	server.Start()

	l.Info("app started", "port", cfg.HTTP.Port, "env", cfg.App.Env, "version", cfg.App.Version)

	select {
	case <-ctx.Done():
		l.Info("app - signal received, shutting down")
	case err := <-server.Notify():
		l.Error("app - server stopped", "error", err.Error())
	}

	if err := server.Shutdown(); err != nil {
		l.Error("app - server shutdown", "error", err.Error())
	}

	for _, p := range workers {
		if err := p.Shutdown(_shutdownTimeout); err != nil {
			l.Error("app - poller shutdown", "error", err.Error())
		}
	}
}
