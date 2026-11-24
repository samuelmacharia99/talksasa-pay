// Command loadtest drives the Talksasa Pay product API (/v1) through the Go
// client (pkg/talksasapay) and reports per-operation latency and errors.
//
//	go run ./cmd/loadtest -base-url http://127.0.0.1:8088 -key-file ./key.txt \
//	    -duration 60s -concurrency 16 -rps 200 \
//	    -mix upsert=20,create=30,get=30,list=10,events=10 -payers 100 -max-p99 500ms
//
// With -webhook-listen it also runs a webhook receiver that verifies every
// delivery (secret from -webhook-secret-file) and, at the end, compares the
// deliveries with the events feed: duplicates (expected occasionally:
// delivery is at least once), missing events and rejected signatures.
// Point the product's webhook URL at the listener before starting.
//
// M-Pesa mix (local stack with the fake Daraja ONLY — prompts against a real
// Daraja would go to real phones): add stk=N (bill + STK push, outcome by
// the phone's last digit, see mpesa.go) and c2b=N (paybill payments posted
// to the fake Daraja's /fake/c2b for the run's payers, half of them
// matching a waiting prompt) with -fake-daraja http://127.0.0.1:8090/.
//
// Exit status: 0 ok; 1 a threshold failed (any 5xx, p99 above -max-p99,
// missing webhook deliveries, rejected webhooks) or the run could not start;
// 2 bad flags.
//
// Never run it against production: it creates payers and payment requests.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/talksasa/talksasa-pay/pkg/talksasapay"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

type config struct {
	baseURL, keyFile         string
	duration                 time.Duration
	concurrency              int
	rps                      float64
	mix                      Mix
	payers                   int
	maxP99, timeout          time.Duration
	retries                  int
	webhookListen            string
	webhookSecretFile        string
	webhookGrace             time.Duration
	webhookSecret, keySecret []byte
	fakeDaraja               string
}

func parseFlags(args []string, stderr io.Writer) (*config, error) {
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := &config{}
	mix := fs.String("mix", defaultMix, "operation weights: op=weight,... ops: "+strings.Join(knownOps, ", "))
	fs.StringVar(&c.baseURL, "base-url", "", "API origin, e.g. http://127.0.0.1:8088 (required)")
	fs.StringVar(&c.keyFile, "key-file", "", "file holding the product API key (required; the key is never a flag value)")
	fs.DurationVar(&c.duration, "duration", 30*time.Second, "how long to generate load")
	fs.IntVar(&c.concurrency, "concurrency", 8, "concurrent workers (0 = only run the webhook receiver)")
	fs.Float64Var(&c.rps, "rps", 0, "aggregate request rate limit, token bucket (0 = unlimited)")
	fs.IntVar(&c.payers, "payers", 50, "distinct payers to spread load over (upserted before the run)")
	fs.DurationVar(&c.maxP99, "max-p99", 2*time.Second, "fail if overall p99 latency exceeds this (0 = no limit)")
	fs.DurationVar(&c.timeout, "timeout", 10*time.Second, "per-attempt HTTP timeout")
	fs.IntVar(&c.retries, "retries", 0, "client retries for safe requests (0 measures raw server behaviour)")
	fs.StringVar(&c.webhookListen, "webhook-listen", "", "address for a webhook receiver, e.g. :9099 (optional)")
	fs.StringVar(&c.webhookSecretFile, "webhook-secret-file", "", "file holding the webhook signing secret (required with -webhook-listen)")
	fs.StringVar(&c.fakeDaraja, "fake-daraja", "", "fake Daraja base URL for the c2b op, e.g. http://127.0.0.1:8090/ (local stack only)")
	fs.DurationVar(&c.webhookGrace, "webhook-grace", 30*time.Second, "how long to wait for outstanding webhook deliveries after the load")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	var err error
	if c.mix, err = ParseMix(*mix); err != nil {
		return nil, err
	}
	switch {
	case c.baseURL == "":
		return nil, errors.New("-base-url is required")
	case c.keyFile == "":
		return nil, errors.New("-key-file is required")
	case c.duration <= 0:
		return nil, errors.New("-duration must be positive")
	case c.concurrency < 0 || c.concurrency > 10000:
		return nil, errors.New("-concurrency must be 0..10000")
	case c.concurrency == 0 && c.webhookListen == "":
		return nil, errors.New("-concurrency 0 only makes sense with -webhook-listen")
	case c.rps < 0:
		return nil, errors.New("-rps must be >= 0")
	case c.payers < 1:
		return nil, errors.New("-payers must be >= 1")
	case c.retries < 0:
		return nil, errors.New("-retries must be >= 0")
	case c.mix.Weight(opC2B) > 0 && c.fakeDaraja == "":
		return nil, errors.New("the c2b op needs -fake-daraja")
	case c.webhookListen != "" && c.webhookSecretFile == "":
		return nil, errors.New("-webhook-listen needs -webhook-secret-file")
	}
	if c.fakeDaraja != "" && !strings.HasSuffix(c.fakeDaraja, "/") {
		c.fakeDaraja += "/"
	}
	if c.keySecret, err = readSecretFile(c.keyFile); err != nil {
		return nil, fmt.Errorf("-key-file: %w", err)
	}
	if c.webhookSecretFile != "" {
		if c.webhookSecret, err = readSecretFile(c.webhookSecretFile); err != nil {
			return nil, fmt.Errorf("-webhook-secret-file: %w", err)
		}
	}
	return c, nil
}

// readSecretFile reads a one-line secret; errors never include its content.
func readSecretFile(path string) ([]byte, error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path is the point of the flag
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(string(b))
	if s == "" || strings.ContainsAny(s, "\r\n") {
		return nil, errors.New("file must contain exactly one non-empty line")
	}
	return []byte(s), nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cfg, err := parseFlags(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "loadtest:", err)
		}
		return 2
	}
	st := newStats()
	client, err := talksasapay.New(cfg.baseURL, string(cfg.keySecret),
		talksasapay.WithMaxRetries(cfg.retries), talksasapay.WithTimeout(cfg.timeout),
		talksasapay.WithUserAgent("talksasa-pay-loadtest"), talksasapay.WithLogger(st.attemptLogger()),
		talksasapay.WithHTTPClient(&http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, MaxIdleConns: cfg.concurrency + 16,
			MaxIdleConnsPerHost: cfg.concurrency + 16, IdleConnTimeout: 90 * time.Second,
		}}))
	if err != nil {
		fmt.Fprintln(stderr, "loadtest:", err)
		return 2
	}
	who, err := client.Whoami(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "loadtest: whoami:", err)
		return 1
	}
	fmt.Fprintf(stdout, "product %s (%s), key %s\n", who.ProductName, who.AccountPrefix, who.KeyID)

	var rc *receiver
	var feedStart string
	if cfg.webhookListen != "" {
		if feedStart, err = feedTail(ctx, client); err != nil {
			fmt.Fprintln(stderr, "loadtest: read events feed:", err)
			return 1
		}
		rc = newReceiver()
		var lc net.ListenConfig
		ln, err := lc.Listen(ctx, "tcp", cfg.webhookListen)
		if err != nil {
			fmt.Fprintln(stderr, "loadtest: webhook listener:", err)
			return 1
		}
		srv := &http.Server{Handler: rc.handler(cfg.webhookSecret), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second}
		go func() { _ = srv.Serve(ln) }()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx) // fresh context: the run context may already be cancelled
		}()
		fmt.Fprintf(stdout, "webhook receiver on %s\n", ln.Addr())
	}

	g := &generator{client: client, mix: cfg.mix, payers: cfg.payers, runID: newRunID(), st: st,
		fakeDaraja: cfg.fakeDaraja, fakeHTTP: newFakeHTTP()}
	var elapsed time.Duration
	if cfg.concurrency > 0 {
		fmt.Fprintf(stdout, "run %s: upserting %d payers...\n", g.runID, cfg.payers)
		if err := g.setup(ctx, cfg.concurrency); err != nil {
			fmt.Fprintln(stderr, "loadtest: setup:", err)
			return 1
		}
		fmt.Fprintf(stdout, "load: %s, %d workers, rps limit %v, mix %v\n", cfg.duration, cfg.concurrency, rpsLabel(cfg.rps), cfg.mix.Ops())
		stopCtx, cancel := context.WithTimeout(ctx, cfg.duration)
		limiter := newTokenBucket(cfg.rps, max(1, int(cfg.rps/10)))
		elapsed = g.run(ctx, stopCtx, cfg.concurrency, limiter)
		cancel()
	} else {
		fmt.Fprintf(stdout, "listening for webhooks for %s\n", cfg.duration)
		t := time.NewTimer(cfg.duration)
		select {
		case <-ctx.Done():
		case <-t.C:
		}
		t.Stop()
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "loadtest: interrupted; partial results follow")
	}

	fmt.Fprintln(stdout)
	res := st.report(stdout, elapsed, cfg.mix.Ops())
	if cfg.mix.Weight(opSTK)+cfg.mix.Weight(opC2B) > 0 {
		fmt.Fprintf(stdout, "m-pesa: %d prompts sent, %d paybill payments (%d aimed at a waiting prompt)\n",
			g.stkSent.Load(), g.c2bSent.Load(), g.c2bMatching.Load())
	}
	failed := false
	if res.fiveXX > 0 {
		fmt.Fprintf(stdout, "FAIL: %d attempts returned 5xx\n", res.fiveXX)
		failed = true
	}
	if cfg.maxP99 > 0 && res.p99 > cfg.maxP99 {
		fmt.Fprintf(stdout, "FAIL: p99 %s above -max-p99 %s\n", ms(res.p99), ms(cfg.maxP99))
		failed = true
	}
	if rc != nil && ctx.Err() == nil {
		wres, err := rc.reconcile(ctx, stdout, func(ctx context.Context) ([]string, error) {
			return feedSince(ctx, client, feedStart)
		}, cfg.webhookGrace)
		if err != nil {
			fmt.Fprintln(stdout, "FAIL: reconcile webhooks:", err)
			failed = true
		}
		if wres.missing > 0 {
			fmt.Fprintf(stdout, "FAIL: %d events never delivered\n", wres.missing)
			failed = true
		}
		if wres.rejected > 0 {
			fmt.Fprintf(stdout, "FAIL: %d webhook deliveries rejected\n", wres.rejected)
			failed = true
		}
	}
	if failed || ctx.Err() != nil {
		return 1
	}
	fmt.Fprintln(stdout, "OK")
	return 0
}

func rpsLabel(r float64) string {
	if r <= 0 {
		return "none"
	}
	return fmt.Sprintf("%.0f/s", r)
}
