package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/talksasa/talksasa-pay/pkg/talksasapay"
)

// tokenBucket limits the aggregate request rate. Callers reserve a token
// and sleep until it is due, so waiting is fair and needs no goroutine.
type tokenBucket struct {
	mu     sync.Mutex
	rate   float64 // tokens per second; <=0 = unlimited
	burst  float64
	tokens float64
	last   time.Time
}

func newTokenBucket(rps float64, burst int) *tokenBucket {
	b := float64(max(burst, 1))
	return &tokenBucket{rate: rps, burst: b, tokens: b, last: time.Now()}
}

// Wait blocks until a token is available or ctx ends.
func (b *tokenBucket) Wait(ctx context.Context) error {
	if b.rate <= 0 {
		return ctx.Err()
	}
	b.mu.Lock()
	now := time.Now()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	b.tokens--
	var wait time.Duration
	if b.tokens < 0 {
		wait = time.Duration(-b.tokens / b.rate * float64(time.Second))
	}
	b.mu.Unlock()
	if wait <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// opStats aggregates one operation's results.
type opStats struct {
	hist   *Histogram
	count  int64
	errs   int64
	byCode map[string]int64
}

// stats collects results from all workers.
type stats struct {
	mu   sync.Mutex
	ops  map[string]*opStats
	http atomic.Int64 // attempts that got a 5xx (including retried ones)
	net  atomic.Int64 // attempts that failed without a response
}

func newStats() *stats { return &stats{ops: map[string]*opStats{}} }

func (s *stats) record(op string, d time.Duration, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.ops[op]
	if o == nil {
		o = &opStats{hist: NewHistogram(), byCode: map[string]int64{}}
		s.ops[op] = o
	}
	o.count++
	o.hist.Record(d)
	if err != nil {
		o.errs++
		o.byCode[errorCode(err)]++
	}
}

// errorCode classifies an error for the report.
func errorCode(err error) string {
	if ae, ok := talksasapay.AsAPIError(err); ok {
		if ae.Code != "" {
			return fmt.Sprintf("%d %s", ae.Status, ae.Code)
		}
		return fmt.Sprintf("%d", ae.Status)
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errSkipped):
		return "skipped"
	}
	return "network"
}

// errSkipped marks an op that could not run (e.g. get with nothing created yet).
var errSkipped = errors.New("skipped")

// idPool remembers recently created payment request ids.
type idPool struct {
	mu  sync.Mutex
	ids []string
	pos int
}

const idPoolSize = 10000

func (p *idPool) add(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ids) < idPoolSize {
		p.ids = append(p.ids, id)
		return
	}
	p.ids[p.pos] = id
	p.pos = (p.pos + 1) % idPoolSize
}

func (p *idPool) pick(r *mrand.Rand) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ids) == 0 {
		return "", false
	}
	return p.ids[r.IntN(len(p.ids))], true
}

// generator runs the load.
type generator struct {
	client      *talksasapay.Client
	mix         Mix
	payers      int
	runID       string
	seq         atomic.Int64
	created     idPool
	cursorMu    sync.Mutex
	eventCursor string
	st          *stats
}

func newRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (g *generator) payerRef(i int) string { return fmt.Sprintf("lt-%s-%d", g.runID, i) }

// setup upserts every payer once so create can target them.
func (g *generator) setup(ctx context.Context, concurrency int) error {
	var wg sync.WaitGroup
	idx := make(chan int)
	var firstErr error
	var mu sync.Mutex
	for range max(concurrency, 1) {
		wg.Go(func() {
			for i := range idx {
				_, _, err := g.client.UpsertPayer(ctx, g.payerRef(i), talksasapay.PayerInput{DisplayName: fmt.Sprintf("Load test payer %d", i)})
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("upsert payer %d: %w", i, err)
					}
					mu.Unlock()
				}
			}
		})
	}
	for i := range g.payers {
		mu.Lock()
		failed := firstErr != nil
		mu.Unlock()
		if failed || ctx.Err() != nil {
			break
		}
		idx <- i
	}
	close(idx)
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return firstErr
}

// exec runs one operation. Calls use ctx (not the run deadline) so in-flight
// requests finish and are measured.
func (g *generator) exec(ctx context.Context, op string, r *mrand.Rand) error {
	c := g.client
	switch op {
	case opUpsert:
		i := r.IntN(g.payers)
		_, _, err := c.UpsertPayer(ctx, g.payerRef(i), talksasapay.PayerInput{
			DisplayName: fmt.Sprintf("Load test payer %d", i), Phones: []string{fmt.Sprintf("07%08d", r.IntN(100_000_000))},
		})
		return err
	case opCreate:
		n := g.seq.Add(1)
		pr, _, err := c.CreatePaymentRequest(ctx, talksasapay.PaymentRequestInput{
			Payer: g.payerRef(r.IntN(g.payers)), Reference: fmt.Sprintf("LT-%s-%d", g.runID, n),
			Description: "Load test", Amount: int64(1 + r.IntN(5000)),
			IdempotencyKey: fmt.Sprintf("lt-%s-%d", g.runID, n),
		})
		if err == nil {
			g.created.add(pr.ID)
		}
		return err
	case opGet:
		id, ok := g.created.pick(r)
		if !ok {
			_, err := c.GetPayer(ctx, g.payerRef(r.IntN(g.payers)))
			return err
		}
		_, err := c.GetPaymentRequest(ctx, id)
		return err
	case opCancel:
		id, ok := g.created.pick(r)
		if !ok {
			return errSkipped
		}
		_, err := c.CancelPaymentRequest(ctx, id)
		return err
	case opList:
		_, err := c.ListPaymentRequestsPage(ctx, talksasapay.ListPaymentRequestsParams{Payer: g.payerRef(r.IntN(g.payers)), Limit: 20})
		return err
	case opTxns:
		_, err := c.ListTransactionsPage(ctx, talksasapay.ListTransactionsParams{Limit: 20})
		return err
	case opWhoami:
		_, err := c.Whoami(ctx)
		return err
	case opEvents:
		g.cursorMu.Lock()
		after := g.eventCursor
		g.cursorMu.Unlock()
		page, err := c.ListEvents(ctx, talksasapay.ListEventsParams{After: after, Limit: 100})
		if err == nil && page.NextCursor != nil {
			g.cursorMu.Lock()
			if g.eventCursor == after {
				g.eventCursor = *page.NextCursor
			}
			g.cursorMu.Unlock()
		}
		return err
	}
	return fmt.Errorf("unknown op %q", op)
}

// run drives concurrency workers until stop ends, returning the elapsed time.
func (g *generator) run(ctx, stop context.Context, concurrency int, limiter *tokenBucket) time.Duration {
	start := time.Now()
	var wg sync.WaitGroup
	for w := range concurrency {
		wg.Go(func() {
			r := mrand.New(mrand.NewPCG(uint64(time.Now().UnixNano()), uint64(w))) //nolint:gosec // load mix, not security
			for stop.Err() == nil {
				if limiter.Wait(stop) != nil {
					return
				}
				op := g.mix.Pick(r)
				t0 := time.Now()
				err := g.exec(ctx, op, r)
				if ctx.Err() != nil {
					return // interrupted: don't count a cancelled call
				}
				g.st.record(op, time.Since(t0), err)
			}
		})
	}
	wg.Wait()
	return time.Since(start)
}

// result is the summary used for the exit status.
type result struct {
	total      int64
	errors     int64
	fiveXX     int64
	network    int64
	p99        time.Duration
	achieved   float64
	perOpCount map[string]int64
}

func (s *stats) report(w io.Writer, elapsed time.Duration, order []string) result {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := result{perOpCount: map[string]int64{}, fiveXX: s.http.Load(), network: s.net.Load()}
	all := NewHistogram()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "op\tcount\terrors\trps\tp50\tp90\tp99\tmax\t")
	ops := append([]string(nil), order...)
	for op := range s.ops {
		if !slices.Contains(ops, op) {
			ops = append(ops, op)
		}
	}
	secs := max(elapsed.Seconds(), 1e-9)
	for _, op := range ops {
		o := s.ops[op]
		if o == nil {
			continue
		}
		all.Merge(o.hist)
		res.total += o.count
		res.errors += o.errs
		res.perOpCount[op] = o.count
		fmt.Fprintf(tw, "%s\t%d\t%d\t%.1f\t%s\t%s\t%s\t%s\t\n", op, o.count, o.errs, float64(o.count)/secs,
			ms(o.hist.Quantile(.5)), ms(o.hist.Quantile(.9)), ms(o.hist.Quantile(.99)), ms(o.hist.Max()))
	}
	res.achieved = float64(res.total) / secs
	res.p99 = all.Quantile(.99)
	fmt.Fprintf(tw, "all\t%d\t%d\t%.1f\t%s\t%s\t%s\t%s\t\n", res.total, res.errors, res.achieved,
		ms(all.Quantile(.5)), ms(all.Quantile(.9)), ms(res.p99), ms(all.Max()))
	_ = tw.Flush()

	if res.errors > 0 {
		fmt.Fprintln(w, "\nerrors by op and code:")
		var lines []string
		for op, o := range s.ops {
			for code, n := range o.byCode {
				lines = append(lines, fmt.Sprintf("  %-8s %-36s %d", op, code, n))
			}
		}
		sort.Strings(lines)
		fmt.Fprintln(w, strings.Join(lines, "\n"))
	}
	fmt.Fprintf(w, "\nelapsed %s, achieved %.1f req/s; attempts with 5xx: %d, network failures: %d\n",
		elapsed.Round(time.Millisecond), res.achieved, res.fiveXX, res.network)
	return res
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond)) }

// attemptLogger counts every 5xx/network attempt, including retried ones,
// so a server error hidden by a successful retry still fails the run.
func (s *stats) attemptLogger() talksasapay.Logger {
	return func(e talksasapay.LogEvent) {
		switch {
		case e.Status >= http.StatusInternalServerError:
			s.http.Add(1)
		case e.Status == 0 && e.Err != nil && !errors.Is(e.Err, context.Canceled):
			s.net.Add(1)
		}
	}
}
