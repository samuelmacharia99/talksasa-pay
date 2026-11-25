package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"golang.org/x/sync/errgroup"

	"github.com/talksasa/talksasa-pay/internal/jobs"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/platform/metrics"
	"github.com/talksasa/talksasa-pay/internal/webhook"
)

// runWorker runs background jobs (River): webhook delivery, maintenance
// (charge expiry, webhook attempt retention) and, when M-Pesa is
// configured, Daraja callback processing and the STK status sweep. Several
// workers may run at once; River elects one leader to schedule the periodic
// jobs.
func runWorker(ctx context.Context, logw io.Writer) error {
	a, err := newApp(ctx, logw, "worker")
	if err != nil {
		return err
	}
	defer a.close()

	reg := metrics.NewRegistry()
	m := jobs.NewMetrics(reg)
	if err := db.RegisterMetrics(reg.Prometheus()); err != nil {
		return err
	}
	// Webhook delivery: SSRF-safe client (no private/internal addresses, no
	// redirects); loopback is never allowed outside tests.
	hc, err := webhook.NewHTTPClient(webhook.ClientOptions{
		Env: a.cfg.Env, Timeout: a.cfg.Worker.WebhookTimeout, AllowLoopback: a.cfg.Worker.WebhookAllowLoopback,
	})
	if err != nil {
		return err
	}
	hookWorker, err := webhook.NewWorker(webhook.Options{
		Pool: a.pool, Secrets: a.apps, Logger: a.logger, Client: hc,
		Env: a.cfg.Env, Timeout: a.cfg.Worker.WebhookTimeout,
	})
	if err != nil {
		return err
	}
	deps := jobs.Deps{
		Pool: a.pool, Events: a.events, Logger: a.logger, Metrics: m,
		AttemptRetention:   a.cfg.Worker.AttemptRetention,
		WebhookConcurrency: a.cfg.Worker.WebhookConcurrency,
		Webhook:            hookWorker,
		MpesaConcurrency:   a.cfg.Worker.MpesaConcurrency,
	}
	if a.stk.Available() {
		deps.STK = a.stk
	}
	client, err := jobs.NewClient(deps, jobs.ClientOptions{ExpiryEvery: a.cfg.Worker.MaintenanceEvery})
	if err != nil {
		return err
	}
	runner := jobs.NewRunner(client, a.logger, m, a.cfg.Worker.ShutdownTimeout)

	// Internal-only listener: /healthz for the container healthcheck and
	// /metrics for Prometheus. Never published to the internet.
	mux := http.NewServeMux()
	mux.Handle("/healthz", runner.HealthHandler())
	mux.Handle("/metrics", reg.Handler())
	healthSrv := httpx.NewServer(a.cfg.Worker.HealthAddr, mux, a.cfg.HTTP, a.logger)

	a.logger.Info("starting worker", slog.String("version", version))
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return runner.Run(gctx) })
	g.Go(func() error { return httpx.Serve(gctx, healthSrv, a.cfg.HTTP.ShutdownTimeout, a.logger) })
	return g.Wait()
}
