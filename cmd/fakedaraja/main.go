// Command fakedaraja serves the in-memory Safaricom Daraja fake
// (internal/mpesa/mpesatest) for local development and smoke tests. It
// refuses to start unless TSP_ENV is empty or "development".
//
// Environment:
//
//	TSP_FAKE_DARAJA_ADDR            listen address (default :8090)
//	TSP_FAKE_DARAJA_CALLBACK_DELAY  delay before STK callbacks (default 300ms)
//	TSP_MPESA_CONSUMER_KEY_FILE, TSP_MPESA_CONSUMER_SECRET_FILE,
//	TSP_MPESA_PASSKEY_FILE          optional: when set, the fake checks the
//	                                client uses these exact credentials
//
// `fakedaraja healthcheck` probes /healthz (for the container healthcheck).
// Phone-number behaviours are documented in package mpesatest.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/talksasa/talksasa-pay/internal/mpesa/mpesatest"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fakedaraja:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	addr := envOr("TSP_FAKE_DARAJA_ADDR", ":8090")
	if len(args) > 0 && args[0] == "healthcheck" {
		return healthcheck(addr)
	}
	if env := strings.TrimSpace(os.Getenv("TSP_ENV")); env != "" && env != "development" {
		return fmt.Errorf("refusing to run with TSP_ENV=%q: the fake Daraja is for development only", env)
	}
	delay, err := time.ParseDuration(envOr("TSP_FAKE_DARAJA_CALLBACK_DELAY", "300ms"))
	if err != nil || delay <= 0 || delay > 10*time.Minute {
		return errors.New("TSP_FAKE_DARAJA_CALLBACK_DELAY: must be a duration between 0 and 10m")
	}
	opts := mpesatest.Options{CallbackDelay: delay}
	for _, s := range []struct {
		env string
		dst *string
	}{
		{"TSP_MPESA_CONSUMER_KEY_FILE", &opts.ConsumerKey},
		{"TSP_MPESA_CONSUMER_SECRET_FILE", &opts.ConsumerSecret},
		{"TSP_MPESA_PASSKEY_FILE", &opts.Passkey},
	} {
		if *s.dst, err = readSecret(s.env); err != nil {
			return err
		}
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	opts.Logger = log

	fake := mpesatest.NewFake(opts)
	defer fake.Close()
	srv := &http.Server{
		Addr:              addr,
		Handler:           fake,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("fake daraja listening", "addr", addr, "callback_delay", delay.String(),
		"verifies_credentials", opts.ConsumerKey != "", "verifies_passkey", opts.Passkey != "")
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

func readSecret(env string) (string, error) {
	path := strings.TrimSpace(os.Getenv(env))
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // path comes from the operator's environment
	if err != nil {
		return "", fmt.Errorf("%s: cannot read secret file", env)
	}
	return strings.TrimSpace(string(b)), nil
}

func healthcheck(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz returned %d", resp.StatusCode)
	}
	return nil
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
