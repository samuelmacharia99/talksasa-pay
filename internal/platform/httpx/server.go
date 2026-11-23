package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/talksasa/talksasa-pay/internal/config"
)

// NewServer builds an http.Server with hardened timeouts.
func NewServer(addr string, h http.Handler, c config.HTTPConfig, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: c.ReadHeaderTimeout,
		ReadTimeout:       c.ReadTimeout,
		WriteTimeout:      c.WriteTimeout,
		IdleTimeout:       c.IdleTimeout,
		MaxHeaderBytes:    32 << 10,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
}

// Serve runs srv until ctx is cancelled, then shuts down gracefully, letting
// in-flight requests finish within timeout.
func Serve(ctx context.Context, srv *http.Server, timeout time.Duration, logger *slog.Logger) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", srv.Addr)
	if err != nil {
		return err
	}
	logger.Info("http server listening", slog.String("addr", ln.Addr().String()))
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	logger.Info("http server shutting down", slog.String("addr", srv.Addr))
	sctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
