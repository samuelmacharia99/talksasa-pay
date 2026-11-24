package integrationtest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
)

// 4. Hundreds of concurrent C2B payments (distinct receipts) for ONE payer
// with several open requests. Untargeted allocation is order-independent, so
// the end state is exactly the greedy oldest-first fill of the total, with
// the rest as credit — whatever order the payments were serialised in.
func TestConcurrencyManyPaymentsOnePayer(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("MANY").Product
	pr := e.payer(p.ID, "c1")

	const nPay = 200
	amounts := make([]int64, nPay)
	var total int64
	for i := range amounts {
		amounts[i] = 20 + int64(i*53)%180
		total += amounts[i]
	}
	// Requests worth more than the payments, so the end state has a paid
	// prefix, one partially paid request and open ones.
	var reqs []paymentrequest.Request
	var reqTotal int64
	for k := 0; reqTotal < total*13/10; k++ {
		amt := 3000 + int64(k*977)%3000
		reqs = append(reqs, e.request(p.ID, pr.ID, amt))
		reqTotal += amt
	}

	began := time.Now()
	results := make([]allocation.Result, nPay)
	errs := storm(nPay, func(i int) error {
		_, res, err := e.payErr(e.ctx, e.ledger, pr, amounts[i], nil, newReceipt())
		results[i] = res
		return err
	})
	took := time.Since(began)
	requireNoErrors(t, "payment", errs)
	t.Logf("one payer, %d requests, concurrent C2B payments: %s", len(reqs), opsPerSec(nPay, took))

	for i, res := range results {
		if res.Duplicate || (allocation.Plan{Lines: res.Lines}).Total() != money.KES(amounts[i]) {
			t.Fatalf("payment %d: result %+v does not allocate exactly %d", i, res, amounts[i])
		}
	}
	remaining := total
	partials := 0
	for i, r := range reqs {
		want := min(remaining, int64(r.Amount))
		remaining -= want
		got := e.reqState(p.ID, r.ID)
		st := allocation.DeriveStatus(r.Amount, money.KES(want), allocation.StatusOpen)
		if st == allocation.StatusPartiallyPaid {
			partials++
		}
		if got.Status != st || int64(got.AmountPaid) != want {
			t.Fatalf("request #%d (created %s): %s paid %d, want %s paid %d (oldest-first violated)",
				i, got.CreatedAt.Format(time.RFC3339Nano), got.Status, got.AmountPaid, st, want)
		}
	}
	if partials > 1 {
		t.Fatalf("%d partially paid requests", partials)
	}
	if c := e.credit(p.ID, pr.ID); int64(c) != remaining {
		t.Fatalf("credit = %d, want %d", c, remaining)
	}
	if c := e.count(`SELECT count(*) FROM allocation_batches WHERE payer_account_id = $1 AND kind = 'payment'`, pr.ID); c != nPay {
		t.Fatalf("payment batches = %d", c)
	}
	e.checkInvariants(invariantOpts{})

	if testing.Short() {
		return
	}
	// Heavy variant: more money than owed (credit appears), targeted STK
	// payments mixed in, and new requests created mid-storm (which apply the
	// credit). Order now matters, so only the invariants are asserted.
	const nMix = 300
	var mixTotal int64
	began = time.Now()
	errs = storm(nMix, func(i int) error {
		switch i % 10 {
		case 0: // new request mid-storm
			_, err := e.createReq(p.ID, pr.ID, fmt.Sprintf("MID-%d", i), fmt.Sprintf("mid-%d", i), 2000, nil)
			return err
		case 1, 2: // STK for a specific request (may already be paid: falls through)
			id := reqs[(i/10)%len(reqs)].ID
			_, _, err := e.payErr(e.ctx, e.ledger, pr, 700, &id, newReceipt())
			return err
		default:
			_, _, err := e.payErr(e.ctx, e.ledger, pr, 250, nil, newReceipt())
			return err
		}
	})
	took = time.Since(began)
	requireNoErrors(t, "mixed storm", errs)
	for i := 0; i < nMix; i++ {
		switch i % 10 {
		case 0:
		case 1, 2:
			mixTotal += 700
		default:
			mixTotal += 250
		}
	}
	t.Logf("mixed storm (payments + targeted + creates): %s", opsPerSec(nMix, took))
	var allocated int64
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT coalesce(sum(amount),0)::bigint FROM allocations WHERE payer_account_id = $1`, pr.ID).Scan(&allocated); err != nil {
		t.Fatal(err)
	}
	if allocated != total+mixTotal {
		t.Fatalf("allocated %d, received %d", allocated, total+mixTotal)
	}
	e.checkInvariants(invariantOpts{})
}

// 5. The same receipt submitted 50 times at once: one transaction, one
// allocation batch, and every caller gets the same idempotent result.
// A receipt replayed with a different amount is refused, never re-booked.
func TestConcurrencySameReceipt(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("RCPT").Product
	pr := e.payer(p.ID, "c1")
	r := e.request(p.ID, pr.ID, 1000)
	receipt := newReceipt()

	const n = 50
	txnIDs := make([]uuid.UUID, n)
	batches := make([]uuid.UUID, n)
	dups := make([]bool, n)
	errs := storm(n, func(i int) error {
		txn, res, err := e.payErr(e.ctx, e.ledger, pr, 1500, nil, receipt)
		txnIDs[i], batches[i], dups[i] = txn.ID, res.BatchID, res.Duplicate
		return err
	})
	requireNoErrors(t, "same receipt", errs)
	fresh := 0
	for i := range txnIDs {
		if txnIDs[i] != txnIDs[0] || batches[i] != batches[0] || batches[i] == uuid.Nil {
			t.Fatalf("caller %d: txn %s batch %s, caller 0: txn %s batch %s", i, txnIDs[i], batches[i], txnIDs[0], batches[0])
		}
		if !dups[i] {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d callers saw a fresh allocation, want 1", fresh)
	}
	if c := e.count(`SELECT count(*) FROM transactions WHERE provider_transaction_id = $1`, receipt); c != 1 {
		t.Fatalf("transactions = %d", c)
	}
	if c := e.count(`SELECT count(*) FROM allocation_batches WHERE transaction_id = $1`, txnIDs[0]); c != 1 {
		t.Fatalf("batches = %d", c)
	}
	assertStatus(t, e.reqState(p.ID, r.ID), allocation.StatusPaid, 1000)
	if c := e.credit(p.ID, pr.ID); c != 500 {
		t.Fatalf("credit = %d", c)
	}

	// Same receipt, two different amounts, concurrently.
	receipt2 := newReceipt()
	amountOf := func(i int) int64 { return 300 + int64(i%2) }
	errs = storm(n, func(i int) error {
		_, _, err := e.payErr(e.ctx, e.ledger, pr, amountOf(i), nil, receipt2)
		return err
	})
	var winAmount int64 = -1
	for i, err := range errs {
		if err == nil {
			if winAmount != -1 && winAmount != amountOf(i) {
				t.Fatalf("receipt booked with two amounts")
			}
			winAmount = amountOf(i)
		}
	}
	for i, err := range errs {
		if amountOf(i) == winAmount && err != nil {
			t.Fatalf("caller %d with the winning amount failed: %v", i, err)
		}
		if amountOf(i) != winAmount && !errors.Is(err, ledger.ErrReceiptConflict) {
			t.Fatalf("caller %d with a different amount: %v, want ErrReceiptConflict", i, err)
		}
	}
	if c := e.count(`SELECT count(*) FROM transactions WHERE provider_transaction_id = $1`, receipt2); c != 1 {
		t.Fatalf("transactions for conflicting receipt = %d", c)
	}
	e.checkInvariants(invariantOpts{})
}

// 8. Reversals race new payments and credit application. A reversal either
// succeeds once, or fails with ErrCreditUsed / ErrAlreadyReversed and writes
// nothing; credit never goes negative.
func TestConcurrencyReversalRacesPaymentsAndCredit(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("REVR").Product

	const n = 30
	type tc struct {
		pr payer.Payer
		r1 paymentrequest.Request
		p1 ledger.Transaction
	}
	cases := make([]tc, n)
	for i := range cases {
		pr := e.payer(p.ID, fmt.Sprintf("c%d", i))
		r1 := e.request(p.ID, pr.ID, 1000)
		p1, _ := e.pay(pr, 1500, nil, newReceipt()) // r1 paid, credit 500
		cases[i] = tc{pr: pr, r1: r1, p1: p1}
	}
	// Setup for the deterministic part below, done before any storm (payer
	// creation is audited; see skipIfAuditWedged).
	pr := e.payer(p.ID, "spent")
	r := e.request(p.ID, pr.ID, 1000)
	pay, _ := e.pay(pr, 1500, nil, newReceipt())
	e.request(p.ID, pr.ID, 400) // consumes 400 of the 500 credit

	// Per case, at once: two reversals of P1, a new request that consumes
	// credit, and a new payment that may replenish it.
	revErr := make([][2]error, n)
	errs := storm(4*n, func(k int) error {
		i := k / 4
		switch k % 4 {
		case 0, 1:
			revErr[i][k%4] = retryAuditFork(func() error {
				_, _, err := e.ledger.Reverse(e.ctx, operator, ledger.ReverseParams{
					OriginalID: cases[i].p1.ID, Reason: "Safaricom reversal", ProviderReceipt: newReceipt(),
				})
				return err
			})
			return nil
		case 2:
			_, err := e.createReq(p.ID, cases[i].pr.ID, fmt.Sprintf("R2-%d", i), fmt.Sprintf("r2-%d", i), 300, nil)
			return err
		default:
			_, _, err := e.payErr(e.ctx, e.ledger, cases[i].pr, 200, nil, newReceipt())
			return err
		}
	})
	requireNoErrors(t, "storm", errs)
	for _, re := range revErr {
		e.skipIfAuditWedged(re[0], re[1])
	}

	var reversed, refused int
	for i, c := range cases {
		ok := 0
		for _, err := range revErr[i] {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, allocation.ErrCreditUsed), errors.Is(err, ledger.ErrAlreadyReversed):
			default:
				t.Fatalf("case %d: reversal error %v", i, err)
			}
		}
		if ok > 1 {
			t.Fatalf("case %d: reversed %d times", i, ok)
		}
		nRev := e.count(`SELECT count(*) FROM transactions WHERE reverses_transaction_id = $1`, c.p1.ID)
		nBatch := e.count(`SELECT count(*) FROM allocation_batches WHERE kind = 'reversal' AND transaction_id IN
			(SELECT id FROM transactions WHERE reverses_transaction_id = $1)`, c.p1.ID)
		if nRev != int64(ok) || nBatch != int64(ok) {
			t.Fatalf("case %d: %d successful reversals but %d reversal rows / %d batches", i, ok, nRev, nBatch)
		}
		r1 := e.reqState(p.ID, c.r1.ID)
		if ok == 1 {
			reversed++
			if r1.Status == allocation.StatusPaid { // reopened; at most P2's 200 can land on it
				t.Fatalf("case %d: reversed but r1 still paid", i)
			}
		} else {
			refused++
			assertStatus(t, r1, allocation.StatusPaid, 1000)
		}
	}
	t.Logf("reversal races: %d reversed, %d refused (credit already used)", reversed, refused)
	e.checkInvariants(invariantOpts{allowCreditWithPayable: true})

	// Deterministic: once the credit is spent, a storm of reversals is
	// refused every time and writes nothing.
	errs = storm(20, func(int) error {
		_, _, err := e.ledger.Reverse(e.ctx, operator, ledger.ReverseParams{OriginalID: pay.ID, Reason: "reversed", ProviderReceipt: newReceipt()})
		return err
	})
	for i, err := range errs {
		if !errors.Is(err, allocation.ErrCreditUsed) {
			t.Fatalf("reversal %d: %v, want ErrCreditUsed", i, err)
		}
	}
	if c := e.count(`SELECT count(*) FROM transactions WHERE reverses_transaction_id = $1`, pay.ID); c != 0 {
		t.Fatalf("refused reversals wrote %d rows", c)
	}
	assertStatus(t, e.reqState(p.ID, r.ID), allocation.StatusPaid, 1000)
	if c := e.credit(p.ID, pr.ID); c != 100 {
		t.Fatalf("credit = %d", c)
	}
	e.checkInvariants(invariantOpts{allowCreditWithPayable: true})
}

// A reversal reopens the requests it had paid. If the product has meanwhile
// created a new live request with the same product reference (allowed: the
// old one was paid), the reversal — money that has ALREADY left via M-Pesa —
// must still be recorded. The reopened request is flagged duplicate_reference
// (exempt from the live-reference index) for staff to resolve; the product
// still cannot create a third live request for that reference.
func TestReversalReopenWithReusedReference(t *testing.T) {
	e := newEnv(t)
	p := e.product("REUSE").Product
	pr := e.payer(p.ID, "c1")
	oldReq, err := e.createReq(p.ID, pr.ID, "INV-7", "k1", 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	pay, _ := e.pay(pr, 1000, nil, newReceipt())
	// Old INV-7 is paid (not live), so the product may bill INV-7 again.
	newReq, err := e.createReq(p.ID, pr.ID, "INV-7", "k2", 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.ledger.Reverse(e.ctx, operator, ledger.ReverseParams{OriginalID: pay.ID, Reason: "Safaricom reversal", ProviderReceipt: newReceipt()}); err != nil {
		t.Fatalf("reversal of money that really left could not be recorded: %v", err)
	}
	assertStatus(t, e.reqState(p.ID, oldReq.Request.ID), allocation.StatusOpen, 0)
	assertStatus(t, e.reqState(p.ID, newReq.Request.ID), allocation.StatusOpen, 0)
	dup := func(id uuid.UUID) int64 {
		return e.count(`SELECT count(*) FROM payment_requests WHERE id = $1 AND duplicate_reference`, id)
	}
	if dup(oldReq.Request.ID) != 1 || dup(newReq.Request.ID) != 0 {
		t.Fatalf("duplicate_reference: old=%d new=%d, want 1/0", dup(oldReq.Request.ID), dup(newReq.Request.ID))
	}
	// The one-live-request-per-reference rule still holds for new requests.
	if _, err := e.createReq(p.ID, pr.ID, "INV-7", "k3", 1000, nil); !errors.Is(err, paymentrequest.ErrDuplicateReference) {
		t.Fatalf("third INV-7: %v, want ErrDuplicateReference", err)
	}
	// Only the allocations trigger may set the flag.
	if _, err := e.db.App.Exec(e.ctx, `UPDATE payment_requests SET duplicate_reference = false WHERE id = $1`, oldReq.Request.ID); err == nil {
		t.Fatal("app role cleared duplicate_reference directly")
	}
	e.checkInvariants(invariantOpts{allowCreditWithPayable: true})
}

// Without a reused reference, a reversal reopens the request normally: not
// flagged, and it keeps blocking a second live request for its reference.
func TestReversalReopenWithoutCollisionIsNotFlagged(t *testing.T) {
	e := newEnv(t)
	p := e.product("REOPEN").Product
	pr := e.payer(p.ID, "c1")
	req, err := e.createReq(p.ID, pr.ID, "INV-8", "k1", 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	pay, _ := e.pay(pr, 1000, nil, newReceipt())
	if _, _, err := e.ledger.Reverse(e.ctx, operator, ledger.ReverseParams{OriginalID: pay.ID, Reason: "Safaricom reversal", ProviderReceipt: newReceipt()}); err != nil {
		t.Fatal(err)
	}
	assertStatus(t, e.reqState(p.ID, req.Request.ID), allocation.StatusOpen, 0)
	if c := e.count(`SELECT count(*) FROM payment_requests WHERE duplicate_reference`); c != 0 {
		t.Fatalf("flagged %d requests", c)
	}
	if _, err := e.createReq(p.ID, pr.ID, "INV-8", "k2", 1000, nil); !errors.Is(err, paymentrequest.ErrDuplicateReference) {
		t.Fatalf("second INV-8: %v, want ErrDuplicateReference", err)
	}
	e.checkInvariants(invariantOpts{allowCreditWithPayable: true})
}
