package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"golang.org/x/sync/errgroup"

	"github.com/talksasa/talksasa-pay/internal/httpapi"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/platform/metrics"
)

func runAPI(ctx context.Context, logw io.Writer) error {
	a, err := newApp(ctx, logw, "api")
	if err != nil {
		return err
	}
	defer a.close()

	reg := metrics.NewRegistry()
	handler := httpapi.NewRouter(httpapi.Deps{
		Logger: a.logger, Pool: a.pool, Products: a.products,
		Metrics: metrics.NewHTTP(reg, "api"), MaxBodyBytes: a.cfg.HTTP.MaxBodyBytes, Version: version,
	})
	apiSrv := httpx.NewServer(a.cfg.HTTP.Addr, handler, a.cfg.HTTP, a.logger)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", reg.Handler())
	metricsSrv := httpx.NewServer(a.cfg.Metrics.Addr, metricsMux, a.cfg.HTTP, a.logger)

	a.logger.Info("starting api", slog.String("version", version))
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return httpx.Serve(gctx, apiSrv, a.cfg.HTTP.ShutdownTimeout, a.logger) })
	g.Go(func() error { return httpx.Serve(gctx, metricsSrv, a.cfg.HTTP.ShutdownTimeout, a.logger) })
	err = g.Wait()
	a.logger.Info("api stopped")
	return err
}
