package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"golang.org/x/sync/errgroup"

	"github.com/talksasa/talksasa-pay/internal/adminapi"
	"github.com/talksasa/talksasa-pay/internal/httpapi"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/platform/metrics"
	"github.com/talksasa/talksasa-pay/internal/webhook"
)

func runAPI(ctx context.Context, logw io.Writer) error {
	a, err := newApp(ctx, logw, "api")
	if err != nil {
		return err
	}
	defer a.close()

	reg := metrics.NewRegistry()
	if err := db.RegisterMetrics(reg.Prometheus()); err != nil {
		return err
	}
	// ready flips to draining on shutdown, before the listener closes;
	// /readyz then answers 503 so the proxy stops routing here.
	ready := &httpx.Readiness{}
	// The STK service follows the M-Pesa settings at run time
	// (Available()); PaybillFrom does the same for the pay page.
	var apiSTK httpapi.STKService = a.stk
	handler := httpapi.NewRouter(httpapi.Deps{
		Logger: a.logger, Pool: a.pool, Apps: a.apps, Reporting: a.reporting,
		Metrics: metrics.NewHTTP(reg, "api"), MaxBodyBytes: a.cfg.HTTP.MaxBodyBytes, Version: version,
		TrustedProxies: a.cfg.TrustedProxies, PaybillFrom: a.mpesa.PaybillShortCode,
		Charges: a.charges, View: a.events.View(),
		KeyLimiter:        httpx.NewRateLimiterPerSecond(a.cfg.API.RatePerKey, a.cfg.API.BurstPerKey),
		APIRequestTimeout: a.cfg.API.RequestTimeout, APIMaxInFlight: a.cfg.API.MaxInFlight, Readiness: ready,
		STK: apiSTK, MpesaCallbackToken: a.cfg.Mpesa.CallbackToken, MpesaCallbackTokenFrom: a.mpesa.CallbackToken, MpesaCallbackCIDRs: a.cfg.Mpesa.CallbackCIDRs,
	})
	apiSrv := httpx.NewServer(a.cfg.HTTP.Addr, handler, a.cfg.HTTP, a.logger)

	// The admin API has its own listener, reachable only from the admin
	// Next.js server's network. It never shares the public listener.
	adminHandler := adminapi.NewRouter(adminapi.Deps{
		Logger: a.logger.With(slog.String("listener", "admin")), Admin: a.admin, Apps: a.apps, Reporting: a.reporting,
		Metrics: metrics.NewHTTP(reg, "admin"), TrustedProxies: a.cfg.TrustedProxies, MaxBodyBytes: 64 << 10,
		Webhooks: webhook.NewDeliveries(a.pool, a.river), STK: a.stk, Ledger: a.ledger, MpesaSettings: a.mpesaSettings,
	})
	adminSrv := httpx.NewServer(a.cfg.Admin.Addr, adminHandler, a.cfg.HTTP, a.logger)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", reg.Handler())
	metricsSrv := httpx.NewServer(a.cfg.Metrics.Addr, metricsMux, a.cfg.HTTP, a.logger)

	a.logger.Info("starting api", slog.String("version", version))
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return httpx.ServeGraceful(gctx, apiSrv, httpx.GracefulOptions{
			ShutdownTimeout: a.cfg.HTTP.ShutdownTimeout, DrainDelay: a.cfg.HTTP.DrainDelay, Readiness: ready,
		}, a.logger)
	})
	g.Go(func() error { return httpx.Serve(gctx, metricsSrv, a.cfg.HTTP.ShutdownTimeout, a.logger) })
	g.Go(func() error { return httpx.Serve(gctx, adminSrv, a.cfg.HTTP.ShutdownTimeout, a.logger) })
	err = g.Wait()
	a.logger.Info("api stopped")
	return err
}
