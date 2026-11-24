package integrationtest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
)

// cleanCtxError reports whether err is what a caller should see when its
// context runs out: the context error itself (pool acquire, client-side
// cancel) or Postgres' query_canceled after pgx's cancel request.
func cleanCtxError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || pgconn.Timeout(err) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "57014"
}

// 12. Far more goroutines than pool connections, each with a short deadline.
// Failures must be clean context errors with no partial writes; retrying
// the failed operations afterwards (same receipts / idempotency keys) books
// each exactly once, and nothing drifts.
func TestConcurrencyPoolExhaustionShortDeadlines(t *testing.T) {
	e := newConcEnv(t, 4)
	p := e.product("POOL").Product
	const nPayers = 8
	payers := make([]payer.Payer, nPayers)
	for i := range payers {
		payers[i] = e.payer(p.ID, fmt.Sprintf("c%d", i))
		e.request(p.ID, payers[i].ID, 5000)
	}

	const n = 160
	receipts := make([]string, n)
	for i := range receipts {
		receipts[i] = newReceipt()
	}
	amount := func(i int) int64 { return 100 + int64(i%7)*10 }
	op := func(ctx context.Context, i int) error {
		pr := payers[i%nPayers]
		if i%5 == 0 {
			_, err := e.requests.Create(ctx, audit.Product(p.ID), p.ID, paymentrequest.CreateParams{
				PayerAccountID: pr.ID, ProductReference: fmt.Sprintf("P-%d", i), Description: "pool",
				Amount: 700, IdempotencyKey: fmt.Sprintf("pool-%d", i),
			})
			return err
		}
		_, _, err := e.payErr(ctx, e.ledger, pr, amount(i), nil, receipts[i])
		return err
	}

	errs := storm(n, func(i int) error {
		ctx, cancel := context.WithTimeout(e.ctx, time.Duration(1+(i%16)*(i%16))*time.Millisecond) // 1..226 ms
		defer cancel()
		return op(ctx, i)
	})
	var failed []int
	for i, err := range errs {
		if err == nil {
			continue
		}
		if !cleanCtxError(err) {
			t.Fatalf("op %d: not a clean context error: %v", i, err)
		}
		failed = append(failed, i)
	}
	t.Logf("pool of 4, %d ops with 1-226 ms deadlines: %d timed out cleanly", n, len(failed))
	if len(failed) == 0 || len(failed) == n {
		t.Fatalf("%d of %d timed out: the scenario needs both outcomes", len(failed), n)
	}

	// No partial writes: every payment that exists is fully allocated, and
	// nothing is left idle in a transaction.
	e.checkInvariants(invariantOpts{})
	deadline := time.Now().Add(5 * time.Second)
	for {
		var idle int64
		if err := e.db.Owner.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND state LIKE 'idle in transaction%'`).Scan(&idle); err != nil {
			t.Fatal(err)
		}
		if idle == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections left idle in transaction", idle)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Retry everything that failed, with a healthy context.
	for _, i := range failed {
		if err := op(e.ctx, i); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	var want int64
	for i := 0; i < n; i++ {
		if i%5 != 0 {
			want += amount(i)
		}
	}
	var got int64
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT coalesce(sum(amount), 0)::bigint FROM transactions WHERE product_id = $1 AND kind = 'payment'`, p.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("booked %d, want %d (a receipt was lost or doubled)", got, want)
	}
	if c := e.count(`SELECT count(*) FROM payment_requests WHERE product_id = $1`, p.ID); c != nPayers+n/5 {
		t.Fatalf("requests = %d, want %d", c, nPayers+n/5)
	}
	e.checkInvariants(invariantOpts{})
}
