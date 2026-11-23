package integrationtest

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/store"
)

func TestExactPayment(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	r := e.request(p.ID, pr.ID, 1000)

	_, res := e.pay(pr, 1000, nil, newReceipt())
	if res.Duplicate || len(res.Requests) != 1 || res.Requests[0].After != allocation.StatusPaid || res.Requests[0].Before != allocation.StatusOpen {
		t.Fatalf("unexpected result %+v", res)
	}
	assertStatus(t, e.reqState(p.ID, r.ID), allocation.StatusPaid, 1000)
	if c := e.credit(p.ID, pr.ID); c != 0 {
		t.Fatalf("credit = %d", c)
	}
	e.assertNoDrift()
}

func TestUnderpaymentThenBalance(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	r := e.request(p.ID, pr.ID, 1000)

	e.pay(pr, 400, nil, newReceipt())
	assertStatus(t, e.reqState(p.ID, r.ID), allocation.StatusPartiallyPaid, 400)
	e.pay(pr, 600, nil, newReceipt())
	assertStatus(t, e.reqState(p.ID, r.ID), allocation.StatusPaid, 1000)
	e.assertNoDrift()
}

func TestOverpaymentCreditsAndAutoAppliesToNewRequests(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	r1 := e.request(p.ID, pr.ID, 1000)

	_, res := e.pay(pr, 1500, nil, newReceipt())
	if res.CreditDelta != 500 || res.CreditBalance != 500 {
		t.Fatalf("credit delta/balance = %d/%d", res.CreditDelta, res.CreditBalance)
	}
	assertStatus(t, e.reqState(p.ID, r1.ID), allocation.StatusPaid, 1000)
	if c := e.credit(p.ID, pr.ID); c != 500 {
		t.Fatalf("credit = %d", c)
	}

	// New request fully covered by credit.
	r2 := e.request(p.ID, pr.ID, 300)
	assertStatus(t, r2, allocation.StatusPaid, 300)
	if c := e.credit(p.ID, pr.ID); c != 200 {
		t.Fatalf("credit after full cover = %d", c)
	}
	// New request partially covered.
	r3 := e.request(p.ID, pr.ID, 1000)
	assertStatus(t, r3, allocation.StatusPartiallyPaid, 200)
	if c := e.credit(p.ID, pr.ID); c != 0 {
		t.Fatalf("credit after partial cover = %d", c)
	}
	e.assertNoDrift()
}

func TestOldestFirstAndTargetFirst(t *testing.T) {
	e := newEnv(t)
	p := e.product("BILL").Product
	pr := e.payer(p.ID, "isp-7")
	old := e.request(p.ID, pr.ID, 1000)
	mid := e.request(p.ID, pr.ID, 1000)
	newest := e.request(p.ID, pr.ID, 1000)

	// C2B (no target): oldest first.
	e.pay(pr, 1200, nil, newReceipt())
	assertStatus(t, e.reqState(p.ID, old.ID), allocation.StatusPaid, 1000)
	assertStatus(t, e.reqState(p.ID, mid.ID), allocation.StatusPartiallyPaid, 200)

	// STK for the newest: target first, surplus oldest-first (mid), then credit.
	target := newest.ID
	e.pay(pr, 2000, &target, newReceipt())
	assertStatus(t, e.reqState(p.ID, newest.ID), allocation.StatusPaid, 1000)
	assertStatus(t, e.reqState(p.ID, mid.ID), allocation.StatusPaid, 1000)
	if c := e.credit(p.ID, pr.ID); c != 200 {
		t.Fatalf("credit = %d", c)
	}
	e.assertNoDrift()
}

func TestLatePaymentForCancelledRequestGoesToCredit(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	r := e.request(p.ID, pr.ID, 1000)
	if _, err := e.requests.Cancel(e.ctx, operator, p.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	target := r.ID
	e.pay(pr, 1000, &target, newReceipt())
	assertStatus(t, e.reqState(p.ID, r.ID), allocation.StatusCancelled, 0)
	if c := e.credit(p.ID, pr.ID); c != 1000 {
		t.Fatalf("credit = %d", c)
	}
	e.assertNoDrift()
}

func TestCannotCancelPartiallyPaid(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	r := e.request(p.ID, pr.ID, 1000)
	e.pay(pr, 100, nil, newReceipt())
	if _, err := e.requests.Cancel(e.ctx, operator, p.ID, r.ID); !errors.Is(err, paymentrequest.ErrNotCancellable) {
		t.Fatalf("cancel partially paid: %v", err)
	}
}

func TestDuplicateReceiptChangesNothing(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	r := e.request(p.ID, pr.ID, 1000)
	receipt := newReceipt()

	t1, first := e.pay(pr, 600, nil, receipt)
	t2, second := e.pay(pr, 600, nil, receipt)
	if t1.ID != t2.ID || !second.Duplicate || first.Duplicate || second.BatchID != first.BatchID {
		t.Fatalf("duplicate not detected: first=%+v second=%+v", first, second)
	}
	assertStatus(t, e.reqState(p.ID, r.ID), allocation.StatusPartiallyPaid, 600)

	// Same receipt arriving on another M-Pesa channel is still the same payment.
	_, _, err := e.ledger.RecordAndAllocate(e.ctx, worker, ledger.IncomingPayment{
		Channel: store.TxnChannelMpesaStk, Receipt: receipt, Amount: 600, ProductID: &pr.ProductID, PayerAccountID: &pr.ID,
	})
	if err != nil {
		t.Fatalf("cross-channel duplicate: %v", err)
	}
	assertStatus(t, e.reqState(p.ID, r.ID), allocation.StatusPartiallyPaid, 600)

	// Same receipt with a different amount is an alarm, not a duplicate.
	_, _, err = e.ledger.RecordAndAllocate(e.ctx, worker, ledger.IncomingPayment{
		Channel: store.TxnChannelMpesaC2b, Receipt: receipt, Amount: 700, ProductID: &pr.ProductID, PayerAccountID: &pr.ID,
	})
	if !errors.Is(err, ledger.ErrReceiptConflict) {
		t.Fatalf("conflicting duplicate: %v", err)
	}

	var n int
	if err := e.db.App.QueryRow(e.ctx, `SELECT count(*) FROM transactions WHERE provider_transaction_id = $1`, receipt).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows for receipt = %d (%v)", n, err)
	}
	e.assertNoDrift()
}

func TestUnmatchedPaymentIsRecordedNotAllocated(t *testing.T) {
	e := newEnv(t)
	receipt := newReceipt()
	txn, res, err := e.ledger.RecordAndAllocate(e.ctx, worker, ledger.IncomingPayment{
		Channel: store.TxnChannelMpesaC2b, Receipt: receipt, Amount: 250, BillRef: "HOST-9999",
	})
	if err != nil {
		t.Fatal(err)
	}
	if txn.PayerAccountID != nil || txn.ProductID != nil || res.BatchID != uuid.Nil {
		t.Fatalf("unmatched payment was allocated: %+v %+v", txn, res)
	}
	if txn.Status != store.TxnStatusSucceeded {
		t.Fatalf("status = %s", txn.Status)
	}
}

func TestConcurrentPaymentsSamePayer(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	var reqs []paymentrequest.Request
	for i := 0; i < 3; i++ {
		reqs = append(reqs, e.request(p.ID, pr.ID, 1000))
	}

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var target *uuid.UUID
			if i%5 == 0 {
				id := reqs[2].ID
				target = &id
			}
			_, _, err := e.ledger.RecordAndAllocate(e.ctx, worker, ledger.IncomingPayment{
				Channel: store.TxnChannelMpesaC2b, Receipt: newReceipt(), Amount: 100,
				ProductID: &pr.ProductID, PayerAccountID: &pr.ID, PaymentRequestID: target,
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent payment: %v", err)
		}
	}
	var paid money.KES
	for _, r := range reqs {
		got := e.reqState(p.ID, r.ID)
		assertStatus(t, got, allocation.StatusPaid, 1000)
		paid += got.AmountPaid
	}
	if c := e.credit(p.ID, pr.ID); c != 5000-paid {
		t.Fatalf("credit = %d, want %d", c, 5000-paid)
	}
	e.assertNoDrift()
}

func TestReversal(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	r := e.request(p.ID, pr.ID, 1000)
	orig, _ := e.pay(pr, 1500, nil, newReceipt())

	if _, _, err := e.ledger.Reverse(e.ctx, worker, ledger.ReverseParams{OriginalID: orig.ID, Reason: "test"}); !errors.Is(err, ledger.ErrActorRequired) {
		t.Fatalf("system actor reversal: %v", err)
	}
	if _, _, err := e.ledger.Reverse(e.ctx, operator, ledger.ReverseParams{OriginalID: orig.ID, Reason: ""}); !errors.Is(err, ledger.ErrReasonRequired) {
		t.Fatalf("no-reason reversal: %v", err)
	}

	rev, res, err := e.ledger.Reverse(e.ctx, operator, ledger.ReverseParams{OriginalID: orig.ID, Reason: "Safaricom reversed: customer dispute", ProviderReceipt: "REV0001"})
	if err != nil {
		t.Fatal(err)
	}
	if rev.Kind != store.TxnKindReversal || rev.Direction != store.TxnDirectionOut || rev.Amount != 1500 || *rev.ReversesTransactionID != orig.ID {
		t.Fatalf("reversal row %+v", rev)
	}
	if res.CreditDelta != -500 {
		t.Fatalf("credit delta = %d", res.CreditDelta)
	}
	assertStatus(t, e.reqState(p.ID, r.ID), allocation.StatusOpen, 0)
	if c := e.credit(p.ID, pr.ID); c != 0 {
		t.Fatalf("credit = %d", c)
	}
	// The original row is untouched.
	got, err := store.New(e.db.App).GetTransactionInternal(e.ctx, orig.ID)
	if err != nil || got.Amount != 1500 || got.Status != store.TxnStatusSucceeded {
		t.Fatalf("original changed: %+v %v", got, err)
	}
	if _, _, err := e.ledger.Reverse(e.ctx, operator, ledger.ReverseParams{OriginalID: orig.ID, Reason: "again"}); !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Fatalf("double reversal: %v", err)
	}
	e.assertNoDrift()
}

func TestReversalRefusedWhenCreditAlreadySpent(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	orig, _ := e.pay(pr, 500, nil, newReceipt()) // all to credit
	e.request(p.ID, pr.ID, 500)                  // credit consumed
	if _, _, err := e.ledger.Reverse(e.ctx, operator, ledger.ReverseParams{OriginalID: orig.ID, Reason: "dispute"}); !errors.Is(err, ledger.ErrReversalReceipt) {
		t.Fatalf("reversal without receipt: %v", err)
	}
	_, _, err := e.ledger.Reverse(e.ctx, operator, ledger.ReverseParams{OriginalID: orig.ID, Reason: "dispute", ProviderReceipt: "REV0002"})
	if !errors.Is(err, allocation.ErrCreditUsed) {
		t.Fatalf("want ErrCreditUsed, got %v", err)
	}
	e.assertNoDrift()
}

func TestPaymentRequestIdempotency(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	params := paymentrequest.CreateParams{
		PayerAccountID: pr.ID, ProductReference: "INV-1", Description: "Hosting", Amount: 2500, IdempotencyKey: "k-1",
	}
	actor := audit.Product(p.ID)

	var wg sync.WaitGroup
	ids := make(chan uuid.UUID, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := e.requests.Create(e.ctx, actor, p.ID, params)
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			ids <- c.Request.ID
		}()
	}
	wg.Wait()
	close(ids)
	var first uuid.UUID
	for id := range ids {
		if first == uuid.Nil {
			first = id
		} else if id != first {
			t.Fatalf("idempotent creates returned different ids")
		}
	}

	changed := params
	changed.Amount = 2600
	if _, err := e.requests.Create(e.ctx, actor, p.ID, changed); !errors.Is(err, paymentrequest.ErrIdempotencyConflict) {
		t.Fatalf("changed params: %v", err)
	}
	dupRef := params
	dupRef.IdempotencyKey = "k-2"
	if _, err := e.requests.Create(e.ctx, actor, p.ID, dupRef); !errors.Is(err, paymentrequest.ErrDuplicateReference) {
		t.Fatalf("duplicate live reference: %v", err)
	}
}
