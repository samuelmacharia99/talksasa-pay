// Command loadtest drives the Talksasa Pay app API (/v1 charges), reports
// per-operation latency and errors, and then checks the money invariants.
//
//	go run ./cmd/loadtest -base-url http://127.0.0.1:8088 -key-file ./key.txt \
//	    -duration 60s -concurrency 16 -rps 200 \
//	    -mix create=40,get=30,list=15,events=15 -max-p99 500ms
//
// Operations (-mix op=weight,...):
//
//	create  POST /charges (no phone)
//	stk     POST /charges with a phone: the first prompt goes out at once and
//	        the fake Daraja decides the outcome by the phone's last digit
//	c2b     a paybill payment posted to the fake Daraja's /fake/c2b: half
//	        pay a live charge of this run (its account number and exact
//	        amount), a quarter use the app's prefix but match nothing
//	        (payment.received), a quarter use -unknown-prefix (unmatched)
//	get, list, events, cancel, txns, whoami
//
// stk and c2b are for the local stack with the fake Daraja ONLY (prompts
// against a real Daraja would go to real phones); c2b needs -fake-daraja.
//
// After the load it waits up to -settle for callbacks and paybill notices to
// land, then checks through /v1 that every charge reached the status it must
// (paid prompt or matched paybill payment -> succeeded), every succeeded
// charge is paid by a succeeded, receipted payment for exactly its amount,
// each receipt is recorded once, every succeeded payment has its
// charge.succeeded or payment.received event and the feed has no event
// twice. With -db-url-file it also runs invariants.sql against the database
// (all apps, unmatched money, webhook deliveries, the audit chain).
//
// With -webhook-listen it also runs a webhook receiver that verifies every
// delivery (secret from -webhook-secret-file) and compares the deliveries
// with the events feed: duplicates, missing events and rejected signatures
// all fail the run. Point the app's webhook URL at the listener first.
//
// Exit status: 0 ok; 1 a threshold or invariant failed (any 5xx, p99 above
// -max-p99, an invariant violation, missing or duplicate webhook deliveries,
// rejected webhooks) or the run could not start; 2 bad flags.
//
// Never run it against production: it creates charges and fake payments.
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
	"regexp"
	"strings"
	"syscall"
	"time"
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
	maxP99, timeout          time.Duration
	retries                  int
	webhookListen            string
	webhookSecretFile        string
	webhookGrace             time.Duration
	webhookSecret, keySecret []byte
	fakeDaraja               string
	unknownPrefix            string
	check                    bool
	settle                   time.Duration
	dbURLFile                string
	dbURL                    []byte
}

var prefixRe = regexp.MustCompile(`^[A-Z]{2,6}$`)

func parseFlags(args []string, stderr io.Writer) (*config, error) {
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := &config{}
	mix := fs.String("mix", defaultMix, "operation weights: op=weight,... ops: "+strings.Join(knownOps, ", "))
	fs.StringVar(&c.baseURL, "base-url", "", "API origin, e.g. http://127.0.0.1:8088 (required)")
	fs.StringVar(&c.keyFile, "key-file", "", "file holding the app API key (required; the key is never a flag value)")
	fs.DurationVar(&c.duration, "duration", 30*time.Second, "how long to generate load")
	fs.IntVar(&c.concurrency, "concurrency", 8, "concurrent workers (0 = only run the webhook receiver)")
	fs.Float64Var(&c.rps, "rps", 0, "aggregate request rate limit, token bucket (0 = unlimited)")
	fs.DurationVar(&c.maxP99, "max-p99", 2*time.Second, "fail if overall p99 latency exceeds this (0 = no limit)")
	fs.DurationVar(&c.timeout, "timeout", 10*time.Second, "per-attempt HTTP timeout")
	fs.IntVar(&c.retries, "retries", 0, "retries for retryable failures (0 measures raw server behaviour)")
	fs.StringVar(&c.webhookListen, "webhook-listen", "", "address for a webhook receiver, e.g. :9099 (optional)")
	fs.StringVar(&c.webhookSecretFile, "webhook-secret-file", "", "file holding the webhook signing secret (required with -webhook-listen)")
	fs.StringVar(&c.fakeDaraja, "fake-daraja", "", "fake Daraja base URL for the c2b op, e.g. http://127.0.0.1:8090/ (local stack only)")
	fs.StringVar(&c.unknownPrefix, "unknown-prefix", "QZLT", "account prefix that belongs to no app (c2b unmatched payments)")
	fs.DurationVar(&c.webhookGrace, "webhook-grace", 30*time.Second, "how long to wait for in-flight webhook deliveries after the load")
	fs.BoolVar(&c.check, "check", true, "check the money invariants after the load")
	fs.DurationVar(&c.settle, "settle", 30*time.Second, "how long to wait for prompts and paybill payments to land before checking")
	fs.StringVar(&c.dbURLFile, "db-url-file", "", "file holding a database URL: also run invariants.sql (optional, read-only)")
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
	case c.retries < 0:
		return nil, errors.New("-retries must be >= 0")
	case c.settle < 0:
		return nil, errors.New("-settle must be >= 0")
	case c.mix.Weight(opC2B) > 0 && c.fakeDaraja == "":
		return nil, errors.New("the c2b op needs -fake-daraja")
	case !prefixRe.MatchString(c.unknownPrefix):
		return nil, errors.New("-unknown-prefix must be 2-6 capital letters")
	case c.webhookListen != "" && c.webhookSecretFile == "":
		return nil, errors.New("-webhook-listen needs -webhook-secret-file")
	}
	c.baseURL = strings.TrimSuffix(c.baseURL, "/")
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
	if c.dbURLFile != "" {
		if c.dbURL, err = readSecretFile(c.dbURLFile); err != nil {
			return nil, fmt.Errorf("-db-url-file: %w", err)
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
	client := &apiClient{
		base: cfg.baseURL, key: string(cfg.keySecret), timeout: cfg.timeout, retries: cfg.retries, onAttempt: st.onAttempt,
		hc: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, MaxIdleConns: cfg.concurrency + 16,
			MaxIdleConnsPerHost: cfg.concurrency + 16, IdleConnTimeout: 90 * time.Second,
		}},
	}
	who, err := client.whoami(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "loadtest: whoami:", err)
		return 1
	}
	fmt.Fprintf(stdout, "app %s (%s), key %s\n", who.AppName, who.AccountPrefix, who.KeyID)
	if who.AccountPrefix == cfg.unknownPrefix {
		fmt.Fprintln(stderr, "loadtest: -unknown-prefix is this app's own prefix")
		return 2
	}

	// The feed position before the run: the invariant checks and the
	// webhook reconciliation look only at this run's events.
	feedStart, err := feedTail(ctx, client)
	if err != nil {
		fmt.Fprintln(stderr, "loadtest: read events feed:", err)
		return 1
	}
	runStart := time.Now()

	var rc *receiver
	if cfg.webhookListen != "" {
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

	expect := &runCharges{}
	expect.init()
	g := &generator{client: client, mix: cfg.mix, runID: newRunID(), prefix: who.AccountPrefix, st: st, expect: expect,
		fakeDaraja: cfg.fakeDaraja, fakeHTTP: newFakeHTTP(), unknownPrefix: cfg.unknownPrefix}
	var elapsed time.Duration
	if cfg.concurrency > 0 {
		fmt.Fprintf(stdout, "run %s: %s, %d workers, rps limit %v, mix %v\n", g.runID, cfg.duration, cfg.concurrency, rpsLabel(cfg.rps), cfg.mix.Ops())
		stopCtx, cancel := context.WithTimeout(ctx, cfg.duration)
		limiter := newTokenBucket(cfg.rps, max(1, int(cfg.rps/10)))
		elapsed = g.run(ctx, stopCtx, cfg.concurrency, limiter)
		cancel()
	} else {
		fmt.Fprintf(stdout, "listening for webhooks for %s\n", cfg.duration)
		if err := sleepCtx(ctx, cfg.duration); err != nil && ctx.Err() == nil {
			return 1
		}
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "loadtest: interrupted; partial results follow")
	}

	fmt.Fprintln(stdout)
	res := st.report(stdout, elapsed, cfg.mix.Ops())
	if cfg.mix.Weight(opSTK)+cfg.mix.Weight(opC2B) > 0 {
		fmt.Fprintf(stdout, "m-pesa: %d prompts sent; %d paybill payments: %d aimed at a live charge, %d own prefix without a match, %d unknown prefix\n",
			g.stkSent.Load(), g.c2bSent.Load(), g.c2bMatching.Load(), g.c2bNoMatch.Load(), g.c2bUnknown.Load())
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
	if ctx.Err() != nil {
		return 1
	}

	if cfg.check && cfg.concurrency > 0 {
		fmt.Fprintln(stdout, "\ninvariants (this run, via /v1):")
		// The checks read every charge of the run, so they too meet the
		// per-key rate limit: wait out 429s instead of failing on them.
		client.retries = max(client.retries, 20)
		v, err := apiInvariants(ctx, client, expect, feedStart, runStart, cfg.settle)
		if err != nil {
			fmt.Fprintln(stdout, "FAIL: invariant checks:", err)
			failed = true
		} else {
			v.report(stdout)
			if n := v.total(); n > 0 {
				fmt.Fprintf(stdout, "FAIL: %d invariant violations\n", n)
				failed = true
			}
		}
	}
	if cfg.check && cfg.dbURL != nil {
		fmt.Fprintln(stdout, "\ninvariants (whole database, invariants.sql):")
		v, err := dbInvariants(ctx, string(cfg.dbURL))
		if err != nil {
			fmt.Fprintln(stdout, "FAIL: database invariants:", err)
			failed = true
		} else {
			v.report(stdout)
			if n := v.total(); n > 0 {
				fmt.Fprintf(stdout, "FAIL: %d database invariant violations\n", n)
				failed = true
			}
		}
	}

	if rc != nil {
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
		if wres.duplicates > 0 {
			fmt.Fprintf(stdout, "FAIL: %d duplicate webhook deliveries (an event delivered twice)\n", wres.duplicates)
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
