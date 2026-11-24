package integrationtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// 1. 200 callers create a request with the same Idempotency-Key and the same
// parameters at the same instant: one row, one id, one created event, one job.
func TestConcurrencySameIdempotencyKey(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("IDEM").Product
	pr := e.payer(p.ID, "c1")
	exp := time.Now().Add(48 * time.Hour)

	const n = 200
	ids := make([]uuid.UUID, n)
	var replays atomic.Int64
	began := time.Now()
	errs := storm(n, func(i int) error {
		c, err := e.createReq(p.ID, pr.ID, "INV-SAME", "same-key", 1500, &exp)
		if err != nil {
			return err
		}
		if c.Replayed {
			replays.Add(1)
		}
		ids[i] = c.Request.ID
		return nil
	})
	took := time.Since(began)
	requireNoErrors(t, "create", errs)
	t.Logf("same-key create storm: %s", opsPerSec(n, took))

	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("caller %d got %s, caller 0 got %s", i, ids[i], ids[0])
		}
	}
	if replays.Load() != n-1 {
		t.Fatalf("replays = %d, want %d", replays.Load(), n-1)
	}
	if c := e.count(`SELECT count(*) FROM payment_requests WHERE product_id = $1`, p.ID); c != 1 {
		t.Fatalf("rows = %d", c)
	}
	if c := e.count(`SELECT count(*) FROM events WHERE product_id = $1 AND type = 'payment_request.created'`, p.ID); c != 1 {
		t.Fatalf("created events = %d", c)
	}
	if c := e.count(`SELECT count(*) FROM river_job j JOIN events ev ON ev.id::text = j.args->>'event_id'
		WHERE j.kind = 'webhook_delivery' AND ev.subject_id = $1`, ids[0]); c != 1 {
		t.Fatalf("river jobs = %d", c)
	}
	e.checkInvariants(invariantOpts{})
}

// 2. Same key, different parameters, concurrently: exactly one winner, every
// other caller gets ErrIdempotencyConflict, and nothing extra is written.
func TestConcurrencyIdempotencyKeyConflict(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("IDEM").Product
	pr := e.payer(p.ID, "c1")

	const n = 100
	errs := storm(n, func(i int) error {
		ref := fmt.Sprintf("INV-%d", i)
		if i%2 == 0 {
			ref = "INV-SHARED" // half also collide on the live reference
		}
		_, err := e.createReq(p.ID, pr.ID, ref, "contested-key", int64(100+i), nil)
		return err
	})
	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, paymentrequest.ErrIdempotencyConflict):
		default:
			t.Fatalf("caller %d: unexpected error %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want 1", winners)
	}
	if c := e.count(`SELECT count(*) FROM payment_requests WHERE product_id = $1`, p.ID); c != 1 {
		t.Fatalf("rows = %d", c)
	}
	if c := e.count(`SELECT count(*) FROM events WHERE product_id = $1`, p.ID); c != 1 {
		t.Fatalf("events = %d", c)
	}
	e.checkInvariants(invariantOpts{})
}

// 3. Different keys, same live product reference: one request is created,
// every other caller gets ErrDuplicateReference.
func TestConcurrencyDuplicateLiveReference(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("DREF").Product
	pr := e.payer(p.ID, "c1")

	const n = 100
	errs := storm(n, func(i int) error {
		_, err := e.createReq(p.ID, pr.ID, "INV-LIVE", fmt.Sprintf("key-%d", i), 1000, nil)
		return err
	})
	created := 0
	for i, err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, paymentrequest.ErrDuplicateReference):
		default:
			t.Fatalf("caller %d: unexpected error %v", i, err)
		}
	}
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	if c := e.count(`SELECT count(*) FROM payment_requests WHERE product_id = $1`, p.ID); c != 1 {
		t.Fatalf("rows = %d", c)
	}
	if c := e.count(`SELECT count(*) FROM events WHERE product_id = $1`, p.ID); c != 1 {
		t.Fatalf("events = %d", c)
	}
	e.checkInvariants(invariantOpts{})
}

// 6. Cancel races a payment on the same request: either it is cancelled with
// nothing paid (the money became credit), or it is paid/partially paid and
// the cancel failed with ErrNotCancellable. Never cancelled-with-money.
func TestConcurrencyCancelRacesPayment(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("CNCL").Product

	const n = 60
	type tc struct {
		pr  payer.Payer
		req paymentrequest.Request
		amt int64
	}
	cases := make([]tc, n)
	for i := range cases {
		pr := e.payer(p.ID, fmt.Sprintf("c%d", i))
		amt := int64(1000)
		if i%2 == 1 {
			amt = 400 // partial
		}
		cases[i] = tc{pr: pr, req: e.request(p.ID, pr.ID, 1000), amt: amt}
	}

	cancelErr := make([]error, n)
	payErr := make([]error, n)
	errs := storm(2*n, func(i int) error {
		c := cases[i/2]
		if i%2 == 0 {
			cancelErr[i/2] = retryAuditFork(func() error {
				_, err := e.requests.Cancel(e.ctx, audit.Product(p.ID), p.ID, c.req.ID)
				return err
			})
			return nil
		}
		id := c.req.ID
		_, _, payErr[i/2] = e.payErr(e.ctx, e.ledger, c.pr, c.amt, &id, newReceipt())
		return nil
	})
	requireNoErrors(t, "storm", errs)
	e.skipIfAuditWedged(cancelErr...)

	var cancelled, paid int
	for i, c := range cases {
		if payErr[i] != nil {
			t.Fatalf("payment %d: %v", i, payErr[i])
		}
		got := e.reqState(p.ID, c.req.ID)
		credit := e.credit(p.ID, c.pr.ID)
		switch {
		case cancelErr[i] == nil:
			cancelled++
			assertStatus(t, got, allocation.StatusCancelled, 0)
			if int64(credit) != c.amt {
				t.Fatalf("case %d: cancelled first, payment should be credit: credit=%d want %d", i, credit, c.amt)
			}
		case errors.Is(cancelErr[i], paymentrequest.ErrNotCancellable):
			paid++
			want := allocation.StatusPaid
			if c.amt < 1000 {
				want = allocation.StatusPartiallyPaid
			}
			assertStatus(t, got, want, money.KES(c.amt))
			if credit != 0 {
				t.Fatalf("case %d: credit=%d", i, credit)
			}
		default:
			t.Fatalf("cancel %d: unexpected %v", i, cancelErr[i])
		}
		nCancelEv := e.count(`SELECT count(*) FROM events WHERE subject_id = $1 AND type = 'payment_request.cancelled'`, c.req.ID)
		if (cancelErr[i] == nil) != (nCancelEv == 1) || nCancelEv > 1 {
			t.Fatalf("case %d: cancelled events = %d (cancel err %v)", i, nCancelEv, cancelErr[i])
		}
	}
	t.Logf("cancel-vs-pay: %d cancelled first, %d paid first", cancelled, paid)
	e.checkInvariants(invariantOpts{})
}

// 7. The expiry sweep races payments aimed at due requests. A request is
// never both expired and paid; money for a request that expired first goes
// to the payer's oldest payable request, then to credit.
func TestConcurrencyExpiryRacesPayment(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("EXPR").Product
	future := time.Now().Add(24 * time.Hour)

	const n = 60
	type tc struct {
		pr     payer.Payer
		old    *paymentrequest.Request // no expiry, older: receives the money if due expired first
		due    paymentrequest.Request
		amount int64
	}
	cases := make([]tc, n)
	for i := range cases {
		pr := e.payer(p.ID, fmt.Sprintf("c%d", i))
		c := tc{pr: pr, amount: 1000}
		if i%2 == 0 {
			o := e.request(p.ID, pr.ID, 500)
			c.old = &o
		}
		cr, err := e.createReq(p.ID, pr.ID, fmt.Sprintf("DUE-%d", i), fmt.Sprintf("due-%d", i), 1000, &future)
		if err != nil {
			t.Fatal(err)
		}
		c.due = cr.Request
		cases[i] = c
	}
	// Make the requests due (expires_at is not a guarded column).
	if _, err := e.db.Owner.Exec(e.ctx, `UPDATE payment_requests SET expires_at = now() - interval '1 second'
		WHERE product_id = $1 AND expires_at IS NOT NULL`, p.ID); err != nil {
		t.Fatal(err)
	}

	var paymentsDone atomic.Bool
	var expiredCount atomic.Int64
	sweep := func() error {
		for {
			var got int
			err := db.WithTx(e.ctx, e.db.App, worker.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
				rows, err := q.ExpireDuePaymentRequests(ctx, 5)
				if err != nil {
					return err
				}
				got = len(rows)
				for _, r := range rows {
					if err := e.events.EmitPaymentRequest(ctx, q, r.ProductID, r.ID, events.PaymentRequestExpired); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			expiredCount.Add(int64(got))
			if got == 0 {
				if paymentsDone.Load() {
					return nil
				}
				time.Sleep(time.Millisecond)
			}
		}
	}

	var wg sync.WaitGroup
	sweepErrs := make([]error, 4)
	for w := range sweepErrs {
		wg.Add(1)
		go func() { defer wg.Done(); sweepErrs[w] = sweep() }()
	}
	payErrs := storm(n, func(i int) error {
		id := cases[i].due.ID
		_, _, err := e.payErr(e.ctx, e.ledger, cases[i].pr, cases[i].amount, &id, newReceipt())
		return err
	})
	paymentsDone.Store(true)
	wg.Wait()
	requireNoErrors(t, "payment", payErrs)
	requireNoErrors(t, "sweep", sweepErrs)

	var expired, paidFirst int
	for i, c := range cases {
		due := e.reqState(p.ID, c.due.ID)
		credit := e.credit(p.ID, c.pr.ID)
		switch due.Status {
		case allocation.StatusExpired:
			expired++
			assertStatus(t, due, allocation.StatusExpired, 0)
			wantCredit := int64(1000)
			if c.old != nil {
				assertStatus(t, e.reqState(p.ID, c.old.ID), allocation.StatusPaid, 500)
				wantCredit = 500
			}
			if int64(credit) != wantCredit {
				t.Fatalf("case %d: expired first: credit %d want %d", i, credit, wantCredit)
			}
		case allocation.StatusPaid:
			paidFirst++
			assertStatus(t, due, allocation.StatusPaid, 1000)
			if c.old != nil {
				assertStatus(t, e.reqState(p.ID, c.old.ID), allocation.StatusOpen, 0)
			}
			if credit != 0 {
				t.Fatalf("case %d: credit %d", i, credit)
			}
		default:
			t.Fatalf("case %d: due request ended %s (paid %d)", i, due.Status, due.AmountPaid)
		}
		if n := e.count(`SELECT count(*) FROM events WHERE subject_id = $1 AND type = 'payment_request.expired'`, c.due.ID); (due.Status == allocation.StatusExpired) != (n == 1) || n > 1 {
			t.Fatalf("case %d: %d expired events for a %s request", i, n, due.Status)
		}
	}
	if int64(expired) != expiredCount.Load() {
		t.Fatalf("sweeps expired %d rows but %d requests are expired", expiredCount.Load(), expired)
	}
	t.Logf("expiry-vs-pay: %d expired first, %d paid first", expired, paidFirst)
	// Old payable requests of payers whose due request was paid keep their
	// outstanding balance while credit is 0: the credit rule holds.
	e.checkInvariants(invariantOpts{})
}
