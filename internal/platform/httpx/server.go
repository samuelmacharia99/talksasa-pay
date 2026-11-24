package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
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

// Readiness is the process-wide "should I receive new traffic" flag. It
// starts ready; shutdown flips it to draining before listeners close, so
// /readyz answers 503 and load balancers stop routing here while in-flight
// requests finish. The zero value is ready and safe for concurrent use.
type Readiness struct {
	draining atomic.Bool
}

// StartDraining marks the process as shutting down. It is idempotent.
func (r *Readiness) StartDraining() {
	if r != nil {
		r.draining.Store(true)
	}
}

// Draining reports whether shutdown has begun. A nil Readiness is never
// draining.
func (r *Readiness) Draining() bool { return r != nil && r.draining.Load() }

// GracefulOptions tunes Serve's shutdown sequence.
type GracefulOptions struct {
	// ShutdownTimeout bounds how long in-flight requests may take to finish
	// after the listener stops accepting. Connections still open after it
	// are closed forcibly.
	ShutdownTimeout time.Duration
	// DrainDelay keeps serving (with Readiness draining and keep-alives
	// off) this long before closing the listener, so health checks notice.
	DrainDelay time.Duration
	// Readiness, if set, is flipped to draining as soon as ctx is done.
	Readiness *Readiness
}

// Serve runs srv until ctx is cancelled, then shuts down gracefully, letting
// in-flight requests finish within timeout.
func Serve(ctx context.Context, srv *http.Server, timeout time.Duration, logger *slog.Logger) error {
	return ServeGraceful(ctx, srv, GracefulOptions{ShutdownTimeout: timeout}, logger)
}

// ServeGraceful runs srv until ctx is cancelled, then shuts down in order:
//
//  1. Readiness flips to draining (/readyz -> 503) and keep-alives are
//     disabled, so clients open new connections elsewhere;
//  2. after DrainDelay, the listener stops accepting and idle connections
//     close (http.Server.Shutdown);
//  3. in-flight requests get up to ShutdownTimeout to finish; whatever is
//     still running then is cut off (http.Server.Close), which cancels
//     those requests' contexts so their DB work rolls back.
func ServeGraceful(ctx context.Context, srv *http.Server, o GracefulOptions, logger *slog.Logger) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", srv.Addr)
	if err != nil {
		return err
	}
	return serveListener(ctx, srv, ln, o, logger)
}

func serveListener(ctx context.Context, srv *http.Server, ln net.Listener, o GracefulOptions, logger *slog.Logger) error {
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
	o.Readiness.StartDraining()
	srv.SetKeepAlivesEnabled(false)
	logger.Info("http server draining", slog.String("addr", ln.Addr().String()), slog.Duration("drain_delay", o.DrainDelay))
	if o.DrainDelay > 0 {
		t := time.NewTimer(o.DrainDelay)
		select {
		case <-t.C:
		case err := <-errCh: // server died while draining
			t.Stop()
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		}
	}
	logger.Info("http server shutting down", slog.String("addr", ln.Addr().String()))
	sctx, cancel := context.WithTimeout(context.Background(), o.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		// Requests outlived the timeout: cut them off rather than hang.
		logger.Warn("http server shutdown timed out; closing remaining connections", slog.String("addr", ln.Addr().String()))
		_ = srv.Close()
		<-errCh
		return err
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
