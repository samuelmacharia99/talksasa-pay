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
)

// runWorker runs background jobs (River): webhook delivery and maintenance
// (payment request expiry, webhook attempt retention). Several workers may
// run at once; River elects one leader to schedule the periodic jobs.
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
	deps := jobs.Deps{
		Pool: a.pool, Events: a.events, Logger: a.logger, Metrics: m,
		AttemptRetention:   a.cfg.Worker.AttemptRetention,
		WebhookConcurrency: a.cfg.Worker.WebhookConcurrency,
		// Webhook: set to the webhook delivery worker (river.Worker[queue.WebhookArgs]).
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
