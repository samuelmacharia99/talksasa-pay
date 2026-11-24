package integrationtest

import (
	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/reporting"
)

// baseInvariants is checkInvariants (concurrency_helpers_test.go) with the
// events checks extended for Phase 2: events may now be about STK pushes
// (subject_type 'stk_push'), which checkEvents does not know yet, so
// checkInvariants reports every stk_push.* event as "subject belongs to
// another product".
func (m *mpesaEnv) baseInvariants(opts invariantOpts) {
	m.t.Helper()
	t, ctx, own := m.t, m.ctx, m.db.Owner
	m.assertNoDrift()
	integ, err := reporting.NewService(m.db.App).CheckIntegrity(ctx)
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
		                  WHERE t.payer_account_id = pa.id AND t.kind = 'payment' AND t.status = 'succeeded'), 0)::bigint,
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
		// Every succeeded matched payment is fully allocated (stricter than
		// checkInvariants, which only sums payments that have a batch).
		if alloc != in-rev {
			t.Fatalf("payer %s: allocations %d != succeeded payments %d - reversals %d", id, alloc, in, rev)
		}
		if !opts.allowCreditWithPayable && credit > 0 && payable > 0 {
			t.Fatalf("payer %s holds credit %d while %d requests are still payable", id, credit, payable)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	m.zero(`matched payments without exactly one payment batch`, `
		SELECT count(*) FROM transactions t
		 WHERE t.kind = 'payment' AND t.status = 'succeeded' AND t.payer_account_id IS NOT NULL
		   AND (SELECT count(*) FROM allocation_batches b WHERE b.transaction_id = t.id AND b.kind = 'payment') <> 1`)
	m.zero(`allocations from a transaction that is not succeeded`, `
		SELECT count(*) FROM allocation_batches b JOIN transactions t ON t.id = b.transaction_id WHERE t.status <> 'succeeded'`)
	m.zero(`closed requests that received money`, `
		SELECT count(*) FROM payment_requests r
		 WHERE r.status IN ('cancelled', 'expired')
		   AND EXISTS (SELECT 1 FROM allocations a WHERE a.payment_request_id = r.id)`)
	m.zero(`over-allocated requests`, `SELECT count(*) FROM payment_requests WHERE amount_paid > amount OR amount_paid < 0`)
	m.zero(`paid/partially paid request without a succeeded transaction allocated to it`, `
		SELECT count(*) FROM payment_requests r WHERE r.amount_paid > 0
		   AND NOT EXISTS (SELECT 1 FROM allocations a JOIN allocation_batches b ON b.id = a.batch_id
		                     JOIN transactions t ON t.id = b.transaction_id
		                    WHERE a.payment_request_id = r.id AND t.status = 'succeeded')`)
	m.zero(`request status disagrees with amount_paid`, `
		SELECT count(*) FROM payment_requests
		 WHERE (status = 'paid') <> (amount_paid = amount)
		    OR (status = 'partially_paid') <> (amount_paid > 0 AND amount_paid < amount AND status NOT IN ('cancelled', 'expired'))`)

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
		m.mpesaCheckEvents(id)
	}
}

// mpesaCheckEvents is checkEvents with stk_push subjects allowed.
func (m *mpesaEnv) mpesaCheckEvents(productID uuid.UUID) {
	m.t.Helper()
	seen := m.drainFeed(productID, 0, uuid.Nil, nil)
	if total := m.count(`SELECT count(*) FROM events WHERE product_id = $1`, productID); int64(len(seen)) != total {
		m.t.Fatalf("product %s: feed returned %d events, table has %d", productID, len(seen), total)
	}
	m.zero("events without exactly one webhook job", `
		SELECT count(*) FROM events ev
		  LEFT JOIN (SELECT args->>'event_id' AS eid, count(*) AS c FROM river_job
		              WHERE kind = 'webhook_delivery' GROUP BY 1) j ON j.eid = ev.id::text
		 WHERE ev.product_id = $1 AND coalesce(j.c, 0) <> 1`, productID)
	m.zero("events without a delivery row", `
		SELECT count(*) FROM events ev WHERE ev.product_id = $1
		   AND NOT EXISTS (SELECT 1 FROM webhook_deliveries d WHERE d.event_id = ev.id AND d.product_id = ev.product_id)`, productID)
	m.zero("webhook jobs for events that do not exist (rolled back?)", `
		SELECT count(*) FROM river_job j WHERE j.kind = 'webhook_delivery'
		   AND NOT EXISTS (SELECT 1 FROM events ev WHERE ev.id::text = j.args->>'event_id')`)
	m.zero("payment batches without exactly one transaction.allocated event", `
		SELECT count(*) FROM allocation_batches b WHERE b.product_id = $1 AND b.kind = 'payment'
		   AND (SELECT count(*) FROM events ev WHERE ev.product_id = $1 AND ev.type = 'transaction.allocated'
		         AND ev.subject_id = b.transaction_id) <> 1`, productID)
	m.zero("requests without exactly one created event", `
		SELECT count(*) FROM payment_requests r WHERE r.product_id = $1
		   AND (SELECT count(*) FROM events ev WHERE ev.product_id = $1 AND ev.type = 'payment_request.created'
		         AND ev.subject_id = r.id) <> 1`, productID)
	m.zero("events whose subject belongs to another product", `
		SELECT count(*) FROM events ev WHERE ev.product_id = $1 AND NOT (
		     (ev.subject_type = 'payment_request' AND EXISTS (SELECT 1 FROM payment_requests r WHERE r.id = ev.subject_id AND r.product_id = ev.product_id))
		  OR (ev.subject_type = 'transaction' AND EXISTS (SELECT 1 FROM transactions t WHERE t.id = ev.subject_id AND t.product_id = ev.product_id))
		  OR (ev.subject_type = 'payer' AND EXISTS (SELECT 1 FROM payer_accounts p WHERE p.id = ev.subject_id AND p.product_id = ev.product_id))
		  OR (ev.subject_type = 'stk_push' AND ev.type LIKE 'stk\_push.%'
		      AND EXISTS (SELECT 1 FROM stk_pushes s WHERE s.id = ev.subject_id AND s.product_id = ev.product_id)))`, productID)
	m.zero("expired/cancelled requests that also reported money", `
		SELECT count(*) FROM payment_requests r WHERE r.product_id = $1 AND r.status IN ('cancelled', 'expired')
		   AND EXISTS (SELECT 1 FROM events ev WHERE ev.subject_id = r.id
		                AND ev.type IN ('payment_request.paid', 'payment_request.partially_paid'))`, productID)
}
