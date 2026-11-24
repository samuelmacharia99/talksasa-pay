package jobs_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// The admin dashboard counts what the M-Pesa jobs leave for staff: unknown
// STK pushes, callbacks that need attention, and payments for inactive
// products or closed payers.
func TestDashboardMpesaCounts(t *testing.T) {
	e := newEnv(t)
	rep := reporting.NewService(e.db.App)
	counts := func() store.DashboardCountsRow {
		t.Helper()
		d, err := rep.Dashboard(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return d.Counts
	}
	if c := counts(); c.UnknownStkPushes != 0 || c.UnprocessedCallbacks != 0 || c.InactiveProductPayments != 0 {
		t.Fatalf("fresh db: %+v", c)
	}

	pid := e.product("DSH")
	pr := e.payer(pid)
	reqID := e.request(pr, 1000, nil)

	// One push that a status check left unknown, one still live.
	unknownPush := func(status store.StkPushStatus) {
		t.Helper()
		err := db.WithTx(e.ctx, e.db.App, payWorker.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
			phone, bill := "254712345678", pr.AccountNumber
			txn, err := q.InsertPendingSTKTransaction(ctx, store.InsertPendingSTKTransactionParams{
				ID: uuid.New(), ProductID: &pid, Amount: 1000, Phone: &phone, BillRef: &bill, PayerAccountID: &pr.ID, PaymentRequestID: &reqID,
			})
			if err != nil {
				return err
			}
			now := time.Now()
			p, err := q.InsertSTKPush(ctx, store.InsertSTKPushParams{ID: uuid.New(), ProductID: pid, PaymentRequestID: reqID,
				TransactionID: txn.ID, Source: "pay_page", Phone: phone, Amount: 1000, NextQueryAt: &now})
			if err != nil {
				return err
			}
			merchant, checkout := "m-"+p.ID.String(), "ws_CO_"+p.ID.String()
			if _, err := q.MarkSTKPushSent(ctx, store.MarkSTKPushSentParams{ID: p.ID, MerchantRequestID: &merchant,
				CheckoutRequestID: &checkout, NextQueryAt: &now}); err != nil {
				return err
			}
			if status == store.StkPushStatusSent {
				return nil
			}
			_, err = q.FinishSTKPush(ctx, store.FinishSTKPushParams{ID: p.ID, Status: status})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	unknownPush(store.StkPushStatusUnknown)
	unknownPush(store.StkPushStatusSent)

	// Callbacks: fresh unprocessed (not yet counted), stale unprocessed,
	// processed with an error recently, processed with an old error, and
	// processed cleanly.
	for _, sql := range []string{
		`INSERT INTO mpesa_callbacks (id, kind, raw) VALUES (gen_random_uuid(), 'stk', '{}')`,
		`INSERT INTO mpesa_callbacks (id, kind, raw, received_at) VALUES (gen_random_uuid(), 'stk', '{}', now() - interval '10 minutes')`,
		`INSERT INTO mpesa_callbacks (id, kind, raw, processed_at, error) VALUES (gen_random_uuid(), 'c2b_confirmation', '{}', now(), 'amount mismatch')`,
		`INSERT INTO mpesa_callbacks (id, kind, raw, received_at, processed_at, error) VALUES (gen_random_uuid(), 'stk', '{}', now() - interval '8 days', now() - interval '8 days', 'old')`,
		`INSERT INTO mpesa_callbacks (id, kind, raw, received_at, processed_at) VALUES (gen_random_uuid(), 'stk', '{}', now() - interval '1 hour', now())`,
	} {
		if _, err := e.db.Owner.Exec(e.ctx, sql); err != nil {
			t.Fatal(err)
		}
	}

	// A payment while the product is active, then the product is suspended:
	// the payment now counts as an inactive-product payment.
	if err := e.pay(pr, 500, nil); err != nil {
		t.Fatal(err)
	}
	if c := counts(); c.InactiveProductPayments != 0 {
		t.Fatalf("active product counted: %+v", c)
	}
	if _, err := e.products.SetStatus(e.ctx, operator, pid, store.ProductStatusSuspended); err != nil {
		t.Fatal(err)
	}

	c := counts()
	if c.UnknownStkPushes != 1 || c.UnprocessedCallbacks != 2 || c.InactiveProductPayments != 1 {
		t.Fatalf("counts: unknown=%d callbacks=%d inactive=%d", c.UnknownStkPushes, c.UnprocessedCallbacks, c.InactiveProductPayments)
	}
}
