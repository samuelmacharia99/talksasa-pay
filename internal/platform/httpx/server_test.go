package httpx

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/internal/config"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestReadinessNilAndZero(t *testing.T) {
	var nilR *Readiness
	nilR.StartDraining()
	if nilR.Draining() {
		t.Fatal("nil readiness draining")
	}
	var r Readiness
	if r.Draining() {
		t.Fatal("zero value should be ready")
	}
	r.StartDraining()
	r.StartDraining()
	if !r.Draining() {
		t.Fatal("not draining")
	}
}

func TestServeGracefulDrainsInFlightThenStops(t *testing.T) {
	ln := listenLocal(t)
	addr := ln.Addr().String()
	ready := &Readiness{}
	entered := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "done")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if ready.Draining() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	srv := NewServer(addr, mux, config.HTTPConfig{ReadHeaderTimeout: time.Second, WriteTimeout: 10 * time.Second}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- serveListener(ctx, srv, ln, GracefulOptions{ShutdownTimeout: 5 * time.Second, DrainDelay: 150 * time.Millisecond, Readiness: ready}, discardLogger())
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	slow := make(chan string, 1)
	go func() {
		resp, err := client.Get("http://" + addr + "/slow")
		if err != nil {
			slow <- "error: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		slow <- string(b)
	}()
	<-entered
	cancel()

	// During the drain delay the listener still accepts, and /readyz is 503.
	deadline := time.Now().Add(time.Second)
	for !ready.Draining() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	resp, err := (&http.Client{Timeout: time.Second}).Get("http://" + addr + "/readyz")
	if err != nil {
		t.Fatalf("readyz during drain: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz during drain = %d", resp.StatusCode)
	}

	time.Sleep(250 * time.Millisecond) // past the drain delay: Shutdown has begun
	close(release)
	if got := <-slow; got != "done" {
		t.Fatalf("in-flight request: %q", got)
	}
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
		_ = c.Close()
		t.Fatal("still accepting after shutdown")
	}
}

func TestServeGracefulForceClosesAfterTimeout(t *testing.T) {
	ln := listenLocal(t)
	addr := ln.Addr().String()
	entered := make(chan struct{})
	handlerCtxDone := make(chan struct{})
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done() // stuck until the connection is cut
		close(handlerCtxDone)
	})
	srv := NewServer(addr, h, config.HTTPConfig{ReadHeaderTimeout: time.Second}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- serveListener(ctx, srv, ln, GracefulOptions{ShutdownTimeout: 100 * time.Millisecond}, discardLogger())
	}()
	go func() {
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered
	cancel()
	select {
	case err := <-served:
		if err == nil {
			t.Fatal("want shutdown timeout error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
	select {
	case <-handlerCtxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stuck handler's context was not cancelled")
	}
}

func TestServeGracefulListenError(t *testing.T) {
	srv := NewServer("256.0.0.1:0", http.NotFoundHandler(), config.HTTPConfig{}, discardLogger())
	if err := Serve(context.Background(), srv, time.Second, discardLogger()); err == nil {
		t.Fatal("want listen error")
	}
}
