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
	"github.com/Calteras/streetfood-customer-api/pkg/httpserver"
	"github.com/Calteras/streetfood-customer-api/pkg/logger"
	"github.com/Calteras/streetfood-customer-api/pkg/postgres"
	"github.com/Calteras/streetfood-customer-api/pkg/secretbox"
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

	pg, err := postgres.New(cfg.PG.URL, postgres.MaxPoolSize(cfg.PG.PoolMax))
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
	disc := discovery.New(store)
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

	hub := ordering.NewHub()
	go hub.Run(ctx, store.ListenOrderUpdates, l)

	server := httpserver.New(httpserver.Port(cfg.HTTP.Port))
	httpctl.NewRouter(server.App, cfg, l, httpctl.Dependencies{
		Auth: ident, Events: integ, Discovery: disc, Profiles: ident, Merchants: merch, Orders: orders, Hub: hub, Payments: pay,
	})

	notifier := ordering.NewNotifier(store, pusher, l)

	workers := []*poller.Poller{
		poller.New("pos events", integ.Process, cfg.Integration.WorkerInterval, l),
		poller.New("pos reconcile", integ.Reconcile, cfg.Integration.ReconcileInterval, l),
		poller.New("order submit", orders.ProcessSubmits, time.Second, l),
		poller.New("order refresh", orders.RefreshStale, 30*time.Second, l),
		poller.New("push", notifier.Process, 2*time.Second, l),
		poller.New("payment check", pay.Poll, 10*time.Second, l),
		poller.New("payment expiry", pay.ExpireUnpaid, 15*time.Second, l),
		poller.New("refunds", pay.Refund, 10*time.Second, l),
	}

	for _, w := range workers {
		w.Start()
	}

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
