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
	if ae, ok := asAPIError(err); ok {
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

// errSkipped marks an op that could not run (e.g. cancel with nothing live).
var errSkipped = errors.New("skipped")

// idPool remembers recently created charge ids.
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

// runCharges records every charge the run created (for the invariant
// checks) and the status some of them must reach.
type runCharges struct {
	mu   sync.Mutex
	all  map[string]bool
	want map[string]string // id -> status it must end in
}

func (rc *runCharges) init() {
	rc.all = map[string]bool{}
	rc.want = map[string]string{}
}

func (rc *runCharges) created(id string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.all[id] = true
}

func (rc *runCharges) add(id, status string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.want[id] = status
}

func (rc *runCharges) snapshot() (all []string, want map[string]string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for id := range rc.all {
		all = append(all, id)
	}
	sort.Strings(all)
	want = make(map[string]string, len(rc.want))
	for k, v := range rc.want {
		want[k] = v
	}
	return all, want
}

// generator runs the load.
type generator struct {
	client      *apiClient
	mix         Mix
	runID       string
	prefix      string // the app's account prefix
	seq         atomic.Int64
	created     idPool
	expect      *runCharges
	cursorMu    sync.Mutex
	eventCursor string
	st          *stats

	// M-Pesa ops.
	fakeDaraja    string       // fake Daraja base URL ending in "/" ("" = no c2b)
	fakeHTTP      *http.Client // for the fake Daraja
	unknownPrefix string       // a prefix no app has (unmatched payments)
	live          livePool
	stkSent       atomic.Int64
	c2bSent       atomic.Int64
	c2bMatching   atomic.Int64
	c2bNoMatch    atomic.Int64
	c2bUnknown    atomic.Int64
}

func newRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// track remembers a created charge.
func (g *generator) track(ch *Charge) {
	g.created.add(ch.ID)
	g.expect.created(ch.ID)
}

// exec runs one operation. Calls use ctx (not the run deadline) so in-flight
// requests finish and are measured.
func (g *generator) exec(ctx context.Context, op string, r *mrand.Rand) error {
	c := g.client
	switch op {
	case opCreate:
		n := g.seq.Add(1)
		ch, err := c.createCharge(ctx, fmt.Sprintf("lt-%s-%d", g.runID, n), ChargeInput{
			Reference: fmt.Sprintf("LT-%s-%d", g.runID, n), Amount: int64(1 + r.IntN(5000)), Description: "Load test",
		})
		if err == nil {
			g.track(ch)
			g.live.add(liveCharge{id: ch.ID, account: ch.AccountNumber, amount: ch.Amount})
		}
		return err
	case opGet:
		id, ok := g.created.pick(r)
		if !ok {
			_, err := c.whoami(ctx)
			return err
		}
		_, err := c.getCharge(ctx, id)
		return err
	case opCancel:
		// Only charges nobody is about to pay, so a c2b aimed at a charge
		// never races with its cancellation.
		lc, ok := g.live.take(r)
		if !ok {
			return errSkipped
		}
		_, err := c.cancelCharge(ctx, lc.id)
		if err == nil {
			g.expect.add(lc.id, "cancelled")
		}
		return err
	case opList:
		status := ""
		if r.IntN(2) == 0 {
			status = "pending"
		}
		_, err := c.listCharges(ctx, status, 20)
		return err
	case opTxns:
		_, err := c.listTransactions(ctx, time.Time{}, "", 20)
		return err
	case opWhoami:
		_, err := c.whoami(ctx)
		return err
	case opSTK:
		return g.stk(ctx, r)
	case opC2B:
		return g.c2b(ctx, r)
	case opEvents:
		g.cursorMu.Lock()
		after := g.eventCursor
		g.cursorMu.Unlock()
		page, err := c.listEvents(ctx, after, 100)
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

// onAttempt counts every 5xx/network attempt, including retried ones, so a
// server error hidden by a successful retry still fails the run.
func (s *stats) onAttempt(status int, err error) {
	switch {
	case status >= http.StatusInternalServerError:
		s.http.Add(1)
	case status == 0 && err != nil && !errors.Is(err, context.Canceled):
		s.net.Add(1)
	}
}
