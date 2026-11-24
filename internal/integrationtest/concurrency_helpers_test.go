package integrationtest

// Shared machinery for the concurrency/chaos suite (concurrency_*_test.go).
//
// Every concurrency test ends with checkInvariants, which proves the money
// core is still consistent after the storm:
//   - cached balances match the allocation lines (no drift) and the audit
//     chain verifies (reporting.CheckIntegrity agrees);
//   - per payer: sum(allocations) == sum(amount_paid) + credit_balance ==
//     sum(allocated incoming payments) - sum(reversals); credit >= 0;
//   - every matched incoming payment has exactly one payment batch;
//   - cancelled/expired requests never received money;
//   - (unless the test allows it) a payer never holds credit while it still
//     has a payable request (credit is always applied oldest-first);
//   - events feed: gap-free and duplicate-free, equals the events table, one
//     webhook delivery row and one River job per event, one
//     transaction.allocated per payment batch, one payment_request.created
//     per request, and every event's subject belongs to its product.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// concPoolSize bounds the app pool of a concurrency env. The dev Postgres
// allows 100 connections shared by every test binary running at once, so the
// storms below deliberately run many more goroutines than connections.
const concPoolSize = 24

// newConcEnv is newEnv with the services rebuilt on an app pool of maxConns
// connections (same role, same settings).
func newConcEnv(t *testing.T, maxConns int32) *env {
	t.Helper()
	e := newEnv(t)
	cfg := e.db.App.Config()
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(e.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close) // runs before the database is dropped (LIFO)
	d := *e.db
	d.App = pool
	e.db = &d
	e.rebuildServices(pool)
	return e
}

// rebuildServices points every service of e at pool.
func (e *env) rebuildServices(pool *pgxpool.Pool) {
	e.t.Helper()
	rc, err := queue.NewInsertOnly(pool, e.logger)
	if err != nil {
		e.t.Fatal(err)
	}
	ev := events.NewEmitter(apiview.View{PayBaseURL: "https://pay.test/", Paybill: "174379"}, queue.NewEnqueuer(rc))
	alloc := allocation.NewService(e.logger, ev)
	e.river, e.events, e.alloc = rc, ev, alloc
	e.payers = payer.NewService(pool, e.logger)
	e.requests = paymentrequest.NewService(pool, alloc, ev, e.logger)
	e.ledger = ledger.NewService(pool, alloc, e.logger)
}

// storm runs fn(i) for i in [0,n) on n goroutines released at the same
// instant, and returns the per-goroutine errors.
func storm(n int, fn func(i int) error) []error {
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

// requireNoErrors fails on the first non-nil error.
func requireNoErrors(t *testing.T, what string, errs []error) {
	t.Helper()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("%s #%d: %v", what, i, err)
		}
	}
}

// pay without t.Fatal, for use on goroutines.
func (e *env) payErr(ctx context.Context, led *ledger.Service, pr payer.Payer, amount int64, target *uuid.UUID, receipt string) (ledger.Transaction, allocation.Result, error) {
	ch := store.TxnChannelMpesaC2b
	if target != nil {
		ch = store.TxnChannelMpesaStk
	}
	return led.RecordAndAllocate(ctx, worker, ledger.IncomingPayment{
		Channel: ch, Receipt: receipt, Amount: money.KES(amount), Phone: "254712345678", BillRef: pr.AccountNumber,
		ProductID: &pr.ProductID, PayerAccountID: &pr.ID, PaymentRequestID: target,
		RawPayload: []byte(`{"TransID":"` + receipt + `"}`),
	})
}

// createReq creates a request without t.Fatal (goroutine-safe).
func (e *env) createReq(productID, payerID uuid.UUID, ref, key string, amount int64, expires *time.Time) (paymentrequest.Created, error) {
	return e.requests.Create(e.ctx, audit.Product(productID), productID, paymentrequest.CreateParams{
		PayerAccountID: payerID, ProductReference: ref, Description: "Invoice " + ref,
		Amount: amount, IdempotencyKey: key, ExpiresAt: expires,
	})
}

type invariantOpts struct {
	// allowCreditWithPayable: a reversal can reopen a request while the
	// payer still holds credit from another payment (credit is only applied
	// on request creation), so reversal tests relax this one rule.
	allowCreditWithPayable bool
}

func (e *env) checkInvariants(opts invariantOpts) {
	e.t.Helper()
	t, ctx, own := e.t, e.ctx, e.db.Owner
	e.assertNoDrift()

	integ, err := reporting.NewService(e.db.App).CheckIntegrity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !integ.AuditChainOK || len(integ.Drift) != 0 {
		t.Fatalf("CheckIntegrity: %+v", integ)
	}

	rows, err := own.Query(ctx, `
		SELECT pa.id, pa.credit_balance,
		       coalesce((SELECT sum(a.amount) FROM allocations a WHERE a.payer_account_id = pa.id), 0)::bigint,
		       coalesce((SELECT sum(r.amount_paid) FROM payment_requests r WHERE r.payer_account_id = pa.id), 0)::bigint,
		       coalesce((SELECT sum(t.amount) FROM transactions t
		                  WHERE t.payer_account_id = pa.id AND t.kind = 'payment'
		                    AND EXISTS (SELECT 1 FROM allocation_batches b WHERE b.transaction_id = t.id AND b.kind = 'payment')), 0)::bigint,
		       coalesce((SELECT sum(t.amount) FROM transactions t WHERE t.payer_account_id = pa.id AND t.kind = 'reversal'), 0)::bigint,
		       (SELECT count(*) FROM payment_requests r WHERE r.payer_account_id = pa.id AND r.status IN ('open', 'partially_paid'))
		  FROM payer_accounts pa`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var credit, alloc, paid, in, rev, payable int64
		if err := rows.Scan(&id, &credit, &alloc, &paid, &in, &rev, &payable); err != nil {
			t.Fatal(err)
		}
		if credit < 0 {
			t.Fatalf("payer %s: negative credit %d", id, credit)
		}
		if alloc != paid+credit {
			t.Fatalf("payer %s: allocations %d != amount_paid %d + credit %d", id, alloc, paid, credit)
		}
		if alloc != in-rev {
			t.Fatalf("payer %s: allocations %d != payments %d - reversals %d", id, alloc, in, rev)
		}
		if !opts.allowCreditWithPayable && credit > 0 && payable > 0 {
			t.Fatalf("payer %s holds credit %d while %d requests are still payable", id, credit, payable)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	e.zero(`matched payments without exactly one payment batch`, `
		SELECT count(*) FROM transactions t
		 WHERE t.kind = 'payment' AND t.status = 'succeeded' AND t.payer_account_id IS NOT NULL
		   AND (SELECT count(*) FROM allocation_batches b WHERE b.transaction_id = t.id AND b.kind = 'payment') <> 1`)
	e.zero(`closed requests that received money`, `
		SELECT count(*) FROM payment_requests r
		 WHERE r.status IN ('cancelled', 'expired')
		   AND EXISTS (SELECT 1 FROM allocations a WHERE a.payment_request_id = r.id)`)
	e.zero(`over-allocated requests`, `SELECT count(*) FROM payment_requests WHERE amount_paid > amount OR amount_paid < 0`)

	prods, err := own.Query(ctx, `SELECT id FROM products`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for prods.Next() {
		var id uuid.UUID
		if err := prods.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	prods.Close()
	for _, id := range ids {
		e.checkEvents(id)
	}
}

// zero fails unless the count query returns 0.
func (e *env) zero(what, sql string, args ...any) {
	e.t.Helper()
	var n int64
	if err := e.db.Owner.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", what, err)
	}
	if n != 0 {
		e.t.Fatalf("%s: %d", what, n)
	}
}

// checkEvents proves the outbox invariants for one product.
func (e *env) checkEvents(productID uuid.UUID) {
	e.t.Helper()
	seen := e.drainFeed(productID, 0, uuid.Nil, nil)
	var total int64
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT count(*) FROM events WHERE product_id = $1`, productID).Scan(&total); err != nil {
		e.t.Fatal(err)
	}
	if int64(len(seen)) != total {
		e.t.Fatalf("product %s: feed returned %d events, table has %d", productID, len(seen), total)
	}
	e.zero("events without exactly one webhook job", `
		SELECT count(*) FROM events ev
		  LEFT JOIN (SELECT args->>'event_id' AS eid, count(*) AS c FROM river_job
		              WHERE kind = 'webhook_delivery' GROUP BY 1) j ON j.eid = ev.id::text
		 WHERE ev.product_id = $1 AND coalesce(j.c, 0) <> 1`, productID)
	e.zero("events without a delivery row", `
		SELECT count(*) FROM events ev WHERE ev.product_id = $1
		   AND NOT EXISTS (SELECT 1 FROM webhook_deliveries d WHERE d.event_id = ev.id AND d.product_id = ev.product_id)`, productID)
	e.zero("webhook jobs for events that do not exist (rolled back?)", `
		SELECT count(*) FROM river_job j WHERE j.kind = 'webhook_delivery'
		   AND NOT EXISTS (SELECT 1 FROM events ev WHERE ev.id::text = j.args->>'event_id')`)
	e.zero("payment batches without exactly one transaction.allocated event", `
		SELECT count(*) FROM allocation_batches b WHERE b.product_id = $1 AND b.kind = 'payment'
		   AND (SELECT count(*) FROM events ev WHERE ev.product_id = $1 AND ev.type = 'transaction.allocated'
		         AND ev.subject_id = b.transaction_id) <> 1`, productID)
	e.zero("requests without exactly one created event", `
		SELECT count(*) FROM payment_requests r WHERE r.product_id = $1
		   AND (SELECT count(*) FROM events ev WHERE ev.product_id = $1 AND ev.type = 'payment_request.created'
		         AND ev.subject_id = r.id) <> 1`, productID)
	e.zero("events whose subject belongs to another product", `
		SELECT count(*) FROM events ev WHERE ev.product_id = $1 AND NOT (
		     (ev.subject_type = 'payment_request' AND EXISTS (SELECT 1 FROM payment_requests r WHERE r.id = ev.subject_id AND r.product_id = ev.product_id))
		  OR (ev.subject_type = 'transaction' AND EXISTS (SELECT 1 FROM transactions t WHERE t.id = ev.subject_id AND t.product_id = ev.product_id))
		  OR (ev.subject_type = 'payer' AND EXISTS (SELECT 1 FROM payer_accounts p WHERE p.id = ev.subject_id AND p.product_id = ev.product_id))
		  OR (ev.subject_type = 'stk_push' AND EXISTS (SELECT 1 FROM stk_pushes s WHERE s.id = ev.subject_id AND s.product_id = ev.product_id)))`, productID)
	e.zero("expired/cancelled requests that also reported money", `
		SELECT count(*) FROM payment_requests r WHERE r.product_id = $1 AND r.status IN ('cancelled', 'expired')
		   AND EXISTS (SELECT 1 FROM events ev WHERE ev.subject_id = r.id
		                AND ev.type IN ('payment_request.paid', 'payment_request.partially_paid'))`, productID)
}

// drainFeed pages ListEventsFeed from the cursor until it has returned every
// event currently in the table for the product. Events written by still-open
// transactions anywhere in the cluster (other test binaries share the server)
// are withheld by design, so it polls for up to 30 s. It fails on duplicates
// and on any out-of-order page, and returns the events it saw (appended to
// into).
func (e *env) drainFeed(productID uuid.UUID, afterTxid int64, afterID uuid.UUID, into []store.Event) []store.Event {
	e.t.Helper()
	seen := make(map[uuid.UUID]bool, len(into))
	for _, ev := range into {
		seen[ev.ID] = true
	}
	deadline := time.Now().Add(30 * time.Second)
	q := store.New(e.db.App)
	for {
		page, err := q.ListEventsFeed(e.ctx, store.ListEventsFeedParams{ProductID: productID, AfterTxid: afterTxid, AfterID: afterID, Lim: 200})
		if err != nil {
			e.t.Fatal(err)
		}
		for _, ev := range page {
			if seen[ev.ID] {
				e.t.Fatalf("feed returned event %s twice", ev.ID)
			}
			if !feedAfter(ev.Txid, ev.ID, afterTxid, afterID) {
				e.t.Fatalf("feed went backwards: (%d,%s) after (%d,%s)", ev.Txid, ev.ID, afterTxid, afterID)
			}
			seen[ev.ID] = true
			into = append(into, ev)
			afterTxid, afterID = ev.Txid, ev.ID
		}
		if len(page) > 0 {
			continue
		}
		var total int
		if err := e.db.Owner.QueryRow(e.ctx, `SELECT count(*) FROM events WHERE product_id = $1`, productID).Scan(&total); err != nil {
			e.t.Fatal(err)
		}
		if len(into) >= total {
			return into
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("feed stuck: saw %d of %d events (a committed event was skipped by the cursor)", len(into), total)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// feedAfter reports whether (txid,id) sorts strictly after the cursor, using
// Postgres uuid ordering (bytewise).
func feedAfter(txid int64, id uuid.UUID, curTxid int64, curID uuid.UUID) bool {
	if txid != curTxid {
		return txid > curTxid
	}
	return bytes.Compare(id[:], curID[:]) > 0
}

// count runs a count query on the owner pool.
func (e *env) count(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.db.Owner.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// opsPerSec formats a throughput figure for t.Logf.
func opsPerSec(n int, d time.Duration) string {
	return fmt.Sprintf("%d ops in %s = %.0f ops/s", n, d.Round(time.Millisecond), float64(n)/d.Seconds())
}

var errRollback = errors.New("deliberate rollback")

// auditForks counts operations that hit the audit-chain append race (see
// TestConcurrencyAuditChainAppends) and were retried.
var auditForks atomic.Int64

// isAuditFork reports the audit-chain append race: two appends picked the
// same predecessor and the prev_id UNIQUE constraint rejected the second.
func isAuditFork(err error) bool { return db.IsUniqueViolation(err, "audit_log_prev_id_key") }

// retryAuditFork retries fn while it fails with the known audit-chain race,
// so the other tests keep testing what they are about. The failed attempt
// rolled back completely (checkInvariants proves it); the race itself is
// demonstrated by TestConcurrencyAuditChainAppends.
func retryAuditFork(fn func() error) error {
	for attempt := 0; ; attempt++ {
		err := fn()
		if !isAuditFork(err) || attempt == 10 {
			return err
		}
		auditForks.Add(1)
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}
}

// skipIfAuditWedged: if the audit-chain race struck so that retries could
// not get through (the chain wedges permanently, see
// TestConcurrencyAuditChainAppends), this run cannot test anything else.
// Skip with the bug rather than fail an unrelated test; money invariants are
// still checked first.
func (e *env) skipIfAuditWedged(errs ...error) {
	e.t.Helper()
	for _, err := range errs {
		if isAuditFork(err) {
			e.checkInvariants(invariantOpts{allowCreditWithPayable: true})
			e.t.Fatalf("audit_log chain wedged (regression of migration 000004): %v", err)
		}
	}
}
