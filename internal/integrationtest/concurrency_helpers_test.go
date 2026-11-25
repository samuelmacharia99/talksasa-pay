package integrationtest

// Shared machinery for the concurrency/chaos suite (concurrency_*_test.go).
//
// Every concurrency test ends with checkInvariants, which proves the core
// is still consistent after the storm:
//   - the audit chain verifies (reporting.CheckIntegrity agrees);
//   - every succeeded charge is paid by a succeeded payment of the same app
//     and charge for exactly its amount; each receipt is recorded once;
//   - events feed: gap-free and duplicate-free, equals the events table, one
//     webhook delivery row and one River job per event, and every event's
//     subject belongs to its app.
//
// The charge event invariants (one charge.succeeded per succeeded charge,
// payment.received for every other app-routed payment, push/charge states)
// are in mpesaInvariants: tests here may seed money without events.

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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/seed"
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
	e.river, e.events = rc, ev
	e.ledger = ledger.NewService(pool, e.logger).WithEvents(ev)
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

// insertCharge inserts a pending charge without t.Fatal (goroutine-safe).
// The same key returns pgx.ErrNoRows (idempotency conflict on insert).
func (e *env) insertCharge(ctx context.Context, appID uuid.UUID, ref, key string, amount int64) (store.Charge, error) {
	e.mu.Lock()
	prefix := e.prefixes[appID]
	e.mu.Unlock()
	p, err := seed.ChargeParams(appID, prefix, amount, seed.ChargeOpts{Reference: ref})
	if err != nil {
		return store.Charge{}, err
	}
	if key != "" {
		p.IdempotencyKey = key
	}
	return store.New(e.db.App).InsertCharge(ctx, p)
}

// paymentErr records a succeeded paybill payment (goroutine-safe) and the
// event the app would get: charge.succeeded when it pays chargeID,
// payment.received otherwise. A duplicate receipt changes nothing.
// Test scaffolding only: the real path is ledger.SucceedCharge.
func (e *env) paymentErr(ctx context.Context, appID, chargeID *uuid.UUID, amount int64, receipt string) (store.Transaction, error) {
	var txn store.Transaction
	err := db.WithTx(ctx, e.db.App, worker.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var created bool
		var err error
		txn, created, err = ledger.RecordIncoming(ctx, q, ledger.IncomingPayment{
			Channel: store.TxnChannelMpesaC2b, Receipt: receipt, Amount: money.KES(amount), Phone: "254712345678",
			AccountNumber: "TEST-1", AppID: appID, ChargeID: chargeID, RawPayload: []byte(`{"TransID":"` + receipt + `"}`),
		})
		if err != nil || !created || appID == nil {
			return err
		}
		if chargeID != nil {
			if _, err := q.MarkChargeSucceeded(ctx, store.MarkChargeSucceededParams{TransactionID: &txn.ID, ID: *chargeID}); err != nil {
				return err
			}
			return e.events.EmitCharge(ctx, q, *appID, *chargeID, events.ChargeSucceeded)
		}
		return e.events.EmitPaymentReceived(ctx, q, *appID, txn.ID, nil)
	})
	return txn, err
}

func (e *env) checkInvariants() {
	e.t.Helper()
	t, ctx, own := e.t, e.ctx, e.db.Owner
	e.assertAuditChain()

	integ, err := reporting.NewService(e.db.App).CheckIntegrity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !integ.AuditChainOK {
		t.Fatalf("CheckIntegrity: %+v", integ)
	}
	e.zero(`succeeded charges not paid by a succeeded payment of exactly their amount`, `
		SELECT count(*) FROM charges c LEFT JOIN transactions t ON t.id = c.transaction_id
		 WHERE c.status IN ('succeeded', 'reversed')
		   AND (t.id IS NULL OR t.status <> 'succeeded' OR t.kind <> 'payment' OR t.amount <> c.amount
		        OR t.charge_id <> c.id OR t.app_id <> c.app_id)`)
	e.zero(`receipt recorded twice`, `
		SELECT count(*) FROM (SELECT provider_transaction_id FROM transactions WHERE provider_transaction_id IS NOT NULL
		                       GROUP BY provider, provider_transaction_id HAVING count(*) > 1) x`)

	apps, err := own.Query(ctx, `SELECT id FROM apps`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for apps.Next() {
		var id uuid.UUID
		if err := apps.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	apps.Close()
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

// checkEvents proves the outbox invariants for one app.
func (e *env) checkEvents(appID uuid.UUID) {
	e.t.Helper()
	seen := e.drainFeed(appID, 0, uuid.Nil, nil)
	var total int64
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT count(*) FROM events WHERE app_id = $1`, appID).Scan(&total); err != nil {
		e.t.Fatal(err)
	}
	if int64(len(seen)) != total {
		e.t.Fatalf("app %s: feed returned %d events, table has %d", appID, len(seen), total)
	}
	e.zero("events without exactly one webhook job", `
		SELECT count(*) FROM events ev
		  LEFT JOIN (SELECT args->>'event_id' AS eid, count(*) AS c FROM river_job
		              WHERE kind = 'webhook_delivery' GROUP BY 1) j ON j.eid = ev.id::text
		 WHERE ev.app_id = $1 AND coalesce(j.c, 0) <> 1`, appID)
	e.zero("events without a delivery row", `
		SELECT count(*) FROM events ev WHERE ev.app_id = $1
		   AND NOT EXISTS (SELECT 1 FROM webhook_deliveries d WHERE d.event_id = ev.id AND d.app_id = ev.app_id)`, appID)
	e.zero("webhook jobs for events that do not exist (rolled back?)", `
		SELECT count(*) FROM river_job j WHERE j.kind = 'webhook_delivery'
		   AND NOT EXISTS (SELECT 1 FROM events ev WHERE ev.id::text = j.args->>'event_id')`)
	e.zero("events whose subject belongs to another app", `
		SELECT count(*) FROM events ev WHERE ev.app_id = $1 AND NOT (
		     (ev.subject_type = 'charge' AND EXISTS (SELECT 1 FROM charges c WHERE c.id = ev.subject_id AND c.app_id = ev.app_id))
		  OR (ev.subject_type = 'transaction' AND EXISTS (SELECT 1 FROM transactions t WHERE t.id = ev.subject_id AND t.app_id = ev.app_id))
		  OR (ev.subject_type = 'stk_push' AND EXISTS (SELECT 1 FROM stk_pushes s WHERE s.id = ev.subject_id AND s.app_id = ev.app_id)))`, appID)
}

// drainFeed pages ListEventsFeed from the cursor until it has returned every
// event currently in the table for the app. Events written by still-open
// transactions anywhere in the cluster (other test binaries share the server)
// are withheld by design, so it polls for up to 30 s. It fails on duplicates
// and on any out-of-order page, and returns the events it saw (appended to
// into).
func (e *env) drainFeed(appID uuid.UUID, afterTxid int64, afterID uuid.UUID, into []store.Event) []store.Event {
	e.t.Helper()
	seen := make(map[uuid.UUID]bool, len(into))
	for _, ev := range into {
		seen[ev.ID] = true
	}
	deadline := time.Now().Add(30 * time.Second)
	q := store.New(e.db.App)
	for {
		page, err := q.ListEventsFeed(e.ctx, store.ListEventsFeedParams{AppID: appID, AfterTxid: afterTxid, AfterID: afterID, Lim: 200})
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
		if err := e.db.Owner.QueryRow(e.ctx, `SELECT count(*) FROM events WHERE app_id = $1`, appID).Scan(&total); err != nil {
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
			e.checkInvariants()
			e.t.Fatalf("audit_log chain wedged (regression of migration 000004): %v", err)
		}
	}
}

// Kept for storms that write audit rows concurrently (see
// TestConcurrencyAuditChainAppends); referenced so the linter keeps them.
var _ = []any{&auditForks, retryAuditFork, (*env).skipIfAuditWedged}
