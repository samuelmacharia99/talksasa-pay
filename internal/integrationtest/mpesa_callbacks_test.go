package integrationtest

// M-Pesa chaos: callbacks racing each other, the sweeper, staff and Send.

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Scenario 1: the same STK success callback delivered 50 times at once
// (each stored separately, processed concurrently, and every stored copy
// processed twice). One succeeded transaction, request paid once, one
// payment_request.paid event. Run on the normal path (push sent) and on
// the orphan path (Daraja's answer was never saved: push initiating).
func TestMpesaDuplicateSuccessCallbackStorm(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		name := "sent"
		if orphan {
			name = "orphan"
		}
		t.Run(name, func(t *testing.T) {
			m := newMpesaEnv(t, nil, nil)
			prod := m.product("DUP").Product.ID
			pa := m.mpesaPayer(prod)
			req := m.request(prod, pa.ID, 1500)
			ph := newPhone(0)
			if orphan {
				m.client.setHook(func(context.Context, mpesa.STKPushRequest, string) error { return transientErr })
			}
			p := m.mustSend(prod, req.ID, ph)
			co, _ := m.client.last()
			if orphan != (p.Status == store.StkPushStatusInitiating) {
				t.Fatalf("setup: push status %s", p.Status)
			}
			body := stkSuccessBody(co, newMpesaReceipt(), 1500, ph)

			const n = 50
			ids := make([]uuid.UUID, n)
			stored := make(chan struct{})
			var storedN atomic.Int64
			start := time.Now()
			errs := storm(n, func(i int) error {
				id, err := m.store(store.MpesaCallbackKindStk, body)
				if err != nil {
					return err
				}
				ids[i] = id
				if storedN.Add(1) == n {
					close(stored)
				}
				if err := m.process(m.ctx, id); err != nil {
					return err
				}
				<-stored
				return m.process(m.ctx, ids[(i+1)%n]) // someone else's copy: must be a no-op
			})
			requireNoErrors(t, "duplicate callback", errs)
			t.Logf("%s; retries=%d", opsPerSec(2*n, time.Since(start)), m.retries.Load())

			got := m.push(p.ID)
			if got.Status != store.StkPushStatusSucceeded {
				t.Fatalf("push status %s", got.Status)
			}
			if n, sum := m.succeededFor(req.ID); n != 1 || sum != 1500 {
				t.Fatalf("succeeded payments for request: %d totalling %d, want 1 of 1500", n, sum)
			}
			assertStatus(t, m.reqState(prod, req.ID), allocation.StatusPaid, 1500)
			if c := m.eventsFor(req.ID, events.PaymentRequestPaid); c != 1 {
				t.Fatalf("payment_request.paid events: %d", c)
			}
			if c := m.eventsFor(p.ID, events.STKPushSucceeded); c != 1 {
				t.Fatalf("stk_push.succeeded events: %d", c)
			}
			if c := m.count(`SELECT count(*) FROM mpesa_callbacks WHERE transaction_id = $1`, p.TransactionID); c != n {
				t.Fatalf("callbacks linked to the transaction: %d, want %d", c, n)
			}
			if c := m.count(`SELECT count(*) FROM transactions WHERE product_id IS NULL`); c != 0 {
				t.Fatalf("%d duplicate callbacks recorded as unmatched money", c)
			}
			m.mpesaInvariants(invariantOpts{})
		})
	}
}

// Scenario 2: the success callback races the sweeper. The status check
// either says "success" (no receipt: needs_review) or nothing definite
// past GiveUpAfter (needs_review). Whatever wins, the callback's receipt
// must end succeeded and paid exactly once; never stuck in needs_review.
func TestMpesaCallbackRacesSweeper(t *testing.T) {
	m := newMpesaEnv(t, nil, func(o *stk.Options) {
		o.QueryAfter = time.Minute
		o.GiveUpAfter = 10 * time.Minute
	})
	prod := m.product("SWP").Product.ID
	pa := m.mpesaPayer(prod)

	const iterations = 40
	var sweeperFirst, callbackFirst int
	for i := 0; i < iterations; i++ {
		amount := int64(100 + i)
		req := m.request(prod, pa.ID, amount)
		ph := newPhone(2)
		p := m.mustSend(prod, req.ID, ph)
		co := *p.CheckoutRequestID
		giveUp := i%2 == 1
		if giveUp {
			m.advance(11 * time.Minute) // past GiveUpAfter; query keeps answering pending
		} else {
			m.client.setQuery(co, mpesa.STKQueryResult{State: mpesa.QuerySucceeded, ResultCode: "0", ResultDesc: "processed"})
			m.advance(2 * time.Minute)
		}
		receipt := newMpesaReceipt()
		var reviewed atomic.Int64
		errs := storm(4, func(g int) error {
			if g == 0 {
				_, err := m.deliver(store.MpesaCallbackKindStk, stkSuccessBody(co, receipt, amount, ph))
				return err
			}
			// Three sweepers at once (SKIP LOCKED: one checks it).
			if i%4 != 0 {
				time.Sleep(time.Duration(rand.IntN(8000)) * time.Microsecond)
			}
			r, err := m.svc.Sweep(m.ctx, 100)
			reviewed.Add(int64(r.NeedsReview))
			return err
		})
		requireNoErrors(t, "race", errs)
		got := m.push(p.ID)
		txn := m.txn(p.TransactionID)
		if got.Status != store.StkPushStatusSucceeded || txn.Status != store.TxnStatusSucceeded ||
			txn.ProviderTransactionID == nil || *txn.ProviderTransactionID != receipt {
			t.Fatalf("iteration %d: push %s, txn %s (receipt %v); want succeeded with %s", i, got.Status, txn.Status, txn.ProviderTransactionID, receipt)
		}
		assertStatus(t, m.reqState(prod, req.ID), allocation.StatusPaid, money.KES(amount))
		switch reviewed.Load() {
		case 0:
			callbackFirst++
			if c := m.eventsFor(p.ID, events.STKPushNeedsReview); c != 0 {
				t.Fatalf("iteration %d: needs_review event although the sweeper lost", i)
			}
		case 1:
			sweeperFirst++
			if c := m.eventsFor(p.ID, events.STKPushNeedsReview); c != 1 {
				t.Fatalf("iteration %d: needs_review events %d", i, c)
			}
		default:
			t.Fatalf("iteration %d: %d sweepers reviewed the same push", i, reviewed.Load())
		}
	}
	t.Logf("sweeper first %d, callback first %d; queries=%d", sweeperFirst, callbackFirst, m.client.queries.Load())
	m.mpesaInvariants(invariantOpts{})
}

// Scenario 3: the success callback arrives while Send is still waiting for
// Daraja (Tx B not committed: push initiating, no checkout id stored), or
// at the same moment Send saves Daraja's answer. Paid exactly once either
// way (orphan path or normal path), one sent and one succeeded event.
func TestMpesaCallbackBeforeSendCommits(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	prod := m.product("EARLY").Product.ID
	pa := m.mpesaPayer(prod)

	type accepted struct {
		checkout string
		req      mpesa.STKPushRequest
	}
	started := make(chan accepted, 1)
	release := make(chan struct{})
	var gate sync.Mutex // one blocked Send at a time
	m.client.setHook(func(_ context.Context, req mpesa.STKPushRequest, co string) error {
		started <- accepted{co, req}
		<-release
		return nil
	})

	const iterations = 30
	modes := map[string]int{}
	for i := 0; i < iterations; i++ {
		gate.Lock()
		amount := int64(200 + i)
		req := m.request(prod, pa.ID, amount)
		ph := newPhone(0)
		type sendRes struct {
			p   stk.Push
			err error
		}
		done := make(chan sendRes, 1)
		go func() {
			p, err := m.sendProduct(prod, req.ID, ph)
			done <- sendRes{p, err}
		}()
		a := <-started
		receipt := newMpesaReceipt()
		body := stkSuccessBody(a.checkout, receipt, a.req.Amount, a.req.Phone)
		dups := 1 + i%4 // 1-4 copies of the callback
		mode := []string{"callback-first", "concurrent", "release-first"}[i%3]
		modes[mode]++
		deliverAll := func() error {
			errs := storm(dups, func(int) error { _, err := m.deliver(store.MpesaCallbackKindStk, body); return err })
			for _, err := range errs {
				if err != nil {
					return err
				}
			}
			return nil
		}
		switch mode {
		case "callback-first":
			if err := deliverAll(); err != nil {
				t.Fatal(err)
			}
			release <- struct{}{}
		case "concurrent":
			var wg sync.WaitGroup
			var derr error
			wg.Add(1)
			go func() { defer wg.Done(); derr = deliverAll() }()
			time.Sleep(time.Duration(rand.IntN(2000)) * time.Microsecond)
			release <- struct{}{}
			wg.Wait()
			if derr != nil {
				t.Fatal(derr)
			}
		case "release-first":
			release <- struct{}{}
			if err := deliverAll(); err != nil {
				t.Fatal(err)
			}
		}
		res := <-done
		gate.Unlock()
		if res.err != nil {
			t.Fatalf("iteration %d (%s): send: %v", i, mode, res.err)
		}
		got := m.push(res.p.ID)
		if got.Status != store.StkPushStatusSucceeded || got.CheckoutRequestID == nil || *got.CheckoutRequestID != a.checkout {
			t.Fatalf("iteration %d (%s): push %s checkout %v", i, mode, got.Status, got.CheckoutRequestID)
		}
		if n, sum := m.succeededFor(req.ID); n != 1 || sum != amount {
			t.Fatalf("iteration %d (%s): %d payments totalling %d", i, mode, n, sum)
		}
		assertStatus(t, m.reqState(prod, req.ID), allocation.StatusPaid, money.KES(amount))
		if c := m.eventsFor(got.ID, events.STKPushSent); c != 1 {
			t.Fatalf("iteration %d (%s): stk_push.sent events %d", i, mode, c)
		}
	}
	if c := m.count(`SELECT count(*) FROM transactions WHERE product_id IS NULL`); c != 0 {
		t.Fatalf("%d early callbacks recorded as unmatched money", c)
	}
	t.Logf("modes %v, retries %d", modes, m.retries.Load())
	m.mpesaInvariants(invariantOpts{})
}

// Scenario 4: the same receipt reaches us as an STK callback and as a C2B
// confirmation at the same time (Safaricom sends both for a paybill STK
// payment). The money is recorded once whichever wins.
func TestMpesaSameReceiptSTKAndC2B(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	prod := m.product("BOTH").Product.ID
	outcomes := map[string]int{}
	for i := 0; i < 30; i++ {
		pa := m.mpesaPayer(prod)
		amount := int64(300 + i)
		req := m.request(prod, pa.ID, amount)
		ph := newPhone(0)
		p := m.mustSend(prod, req.ID, ph)
		receipt := newMpesaReceipt()
		stkBody := stkSuccessBody(*p.CheckoutRequestID, receipt, amount, ph)
		c2b := c2bBody(receipt, amount, pa.AccountNumber)
		errs := storm(4, func(g int) error {
			if g%2 == 0 {
				_, err := m.deliver(store.MpesaCallbackKindStk, stkBody)
				return err
			}
			time.Sleep(time.Duration(rand.IntN(6000*(i%2)+1)) * time.Microsecond)
			_, err := m.deliver(store.MpesaCallbackKindC2bConfirmation, c2b)
			return err
		})
		requireNoErrors(t, "deliver", errs)
		if c := m.receiptRows(receipt); c != 1 {
			t.Fatalf("iteration %d: receipt on %d rows", i, c)
		}
		var payerMoney int64
		if err := m.db.Owner.QueryRow(m.ctx, `SELECT coalesce(sum(amount),0)::bigint FROM transactions
			WHERE payer_account_id = $1 AND status = 'succeeded'`, pa.ID).Scan(&payerMoney); err != nil {
			t.Fatal(err)
		}
		if payerMoney != amount {
			t.Fatalf("iteration %d: payer received %d, want %d", i, payerMoney, amount)
		}
		assertStatus(t, m.reqState(prod, req.ID), allocation.StatusPaid, money.KES(amount))
		got := m.push(p.ID)
		switch got.Status {
		case store.StkPushStatusSucceeded:
			outcomes["stk won"]++
		case store.StkPushStatusUnknown:
			outcomes["c2b won (push needs review)"]++
		default:
			t.Fatalf("iteration %d: push %s", i, got.Status)
		}
	}
	t.Logf("outcomes %v, retries %d", outcomes, m.retries.Load())
	m.mpesaInvariants(invariantOpts{})
}

// Scenario 7: Safaricom reports a different amount than the push asked
// for. needs_review, no money, an alarm; duplicates and a concurrent sweep
// change nothing; staff cannot confirm it.
func TestMpesaAmountMismatch(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	prod := m.product("MISM").Product.ID
	pa := m.mpesaPayer(prod)
	req := m.request(prod, pa.ID, 1000)
	ph := newPhone(0)
	p := m.mustSend(prod, req.ID, ph)
	co := *p.CheckoutRequestID
	m.client.setQuery(co, mpesa.STKQueryResult{State: mpesa.QuerySucceeded, ResultCode: "0"})
	m.advance(2 * time.Minute)
	receipt := newMpesaReceipt()
	body := stkSuccessBody(co, receipt, 999, ph)
	errs := storm(12, func(g int) error {
		if g < 2 {
			_, err := m.svc.Sweep(m.ctx, 10)
			return err
		}
		_, err := m.deliver(store.MpesaCallbackKindStk, body)
		return err
	})
	requireNoErrors(t, "mismatch", errs)

	got, txn := m.push(p.ID), m.txn(p.TransactionID)
	if got.Status != store.StkPushStatusUnknown || txn.Status != store.TxnStatusNeedsReview {
		t.Fatalf("push %s txn %s, want unknown/needs_review", got.Status, txn.Status)
	}
	if m.receiptRows(receipt) != 0 {
		t.Fatal("mismatched receipt recorded")
	}
	if n, _ := m.succeededFor(req.ID); n != 0 {
		t.Fatalf("%d payments recorded", n)
	}
	assertStatus(t, m.reqState(prod, req.ID), allocation.StatusOpen, 0)
	if m.alarms("amount") == 0 {
		t.Fatal("no alarm logged for the amount mismatch")
	}
	if c := m.eventsFor(p.ID, events.STKPushNeedsReview); c != 1 {
		t.Fatalf("needs_review events %d", c)
	}
	// If the sweeper won, the failure code stays no_receipt (see
	// TestMpesaAmountMismatchAfterSweeperReview); if the callback won,
	// staff cannot confirm.
	t.Logf("failure_code=%s", deref(txn.FailureCode))
	if deref(txn.FailureCode) == "amount_mismatch" {
		_, err := m.svc.Resolve(m.ctx, mpesaStaff(), stk.ResolveParams{TransactionID: txn.ID, Confirm: true, Receipt: receipt, Reason: "portal says paid"})
		if !errors.Is(err, stk.ErrAmountMismatchConfirm) {
			t.Fatalf("confirm after mismatch: %v", err)
		}
	}
	m.mpesaInvariants(invariantOpts{})
}

// Scenario 7b: when the sweeper sends the push to review first ("paid, no
// receipt") and the mismatched callback arrives after, staff must still be
// stopped from confirming the push's (wrong) amount.
func TestMpesaAmountMismatchAfterSweeperReview(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	prod := m.product("MISB").Product.ID
	pa := m.mpesaPayer(prod)
	req := m.request(prod, pa.ID, 1000)
	ph := newPhone(0)
	p := m.mustSend(prod, req.ID, ph)
	co := *p.CheckoutRequestID
	m.client.setQuery(co, mpesa.STKQueryResult{State: mpesa.QuerySucceeded, ResultCode: "0"})
	m.advance(2 * time.Minute)
	if r, err := m.svc.Sweep(m.ctx, 10); err != nil || r.NeedsReview != 1 {
		t.Fatalf("sweep: %+v %v", r, err)
	}
	receipt := newMpesaReceipt()
	m.mustDeliver(store.MpesaCallbackKindStk, stkSuccessBody(co, receipt, 10, ph)) // customer paid KES 10, not 1000
	if m.alarms("amount") == 0 {
		t.Fatal("no alarm for the mismatch")
	}
	txn := m.txn(p.TransactionID)
	_, err := m.svc.Resolve(m.ctx, mpesaStaff(), stk.ResolveParams{TransactionID: txn.ID, Confirm: true, Receipt: receipt, Reason: "portal shows the receipt"})
	if err == nil {
		t.Fatalf("staff confirmed KES %d with receipt %s although Safaricom reported KES 10 for it (failure_code=%s); request now %+v",
			txn.Amount, receipt, deref(txn.FailureCode), m.reqState(prod, req.ID).Status)
	}
	m.mpesaInvariants(invariantOpts{})
}

// Scenario 9: staff resolve a needs_review payment while the late callback
// for it arrives. Money recorded once whichever path wins; the loser gets
// a clean error or a no-op.
func TestMpesaResolveRacesLateCallback(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	prod := m.product("RES").Product.ID
	pa := m.mpesaPayer(prod)
	outcomes := map[string]int{}
	var mu sync.Mutex
	note := func(k string) { mu.Lock(); outcomes[k]++; mu.Unlock() }

	for i := 0; i < 36; i++ {
		amount := int64(400 + i)
		req := m.request(prod, pa.ID, amount)
		ph := newPhone(2)
		p := m.mustSend(prod, req.ID, ph)
		co := *p.CheckoutRequestID
		m.client.setQuery(co, mpesa.STKQueryResult{State: mpesa.QuerySucceeded, ResultCode: "0"})
		m.advance(2 * time.Minute)
		if r, err := m.svc.Sweep(m.ctx, 10); err != nil || r.NeedsReview != 1 {
			t.Fatalf("sweep: %+v %v", r, err)
		}
		receipt := newMpesaReceipt()
		staffReceipt, confirm := receipt, true
		kind := []string{"same-receipt", "staff-typo", "staff-fail"}[i%3]
		switch kind {
		case "staff-typo":
			staffReceipt = newMpesaReceipt()
		case "staff-fail":
			confirm = false
		}
		errs := storm(4, func(g int) error {
			if g == 0 {
				_, err := m.deliver(store.MpesaCallbackKindStk, stkSuccessBody(co, receipt, amount, ph))
				return err
			}
			time.Sleep(time.Duration(rand.IntN(8000*((i/3)%2)+1)) * time.Microsecond)
			_, err := m.svc.Resolve(m.ctx, mpesaStaff(), stk.ResolveParams{TransactionID: p.TransactionID, Confirm: confirm, Receipt: staffReceipt, Reason: "checked the portal"})
			switch {
			case err == nil:
				note(kind + ": resolve won")
			case errors.Is(err, stk.ErrNotResolvable):
				note(kind + ": resolve lost (not_resolvable)")
			default:
				return err
			}
			return nil
		})
		requireNoErrors(t, kind, errs)
		if n, sum := m.succeededFor(req.ID); n != 1 || sum != amount {
			t.Fatalf("iteration %d (%s): %d payments totalling %d", i, kind, n, sum)
		}
		assertStatus(t, m.reqState(prod, req.ID), allocation.StatusPaid, money.KES(amount))
		if got := m.push(p.ID); got.Status != store.StkPushStatusSucceeded {
			t.Fatalf("iteration %d (%s): push %s", i, kind, got.Status)
		}
		if c := m.count(`SELECT count(*) FROM audit_log WHERE action = 'transaction.resolved' AND entity_id = $1`, p.TransactionID.String()); c > 1 {
			t.Fatalf("iteration %d (%s): resolved %d times", i, kind, c)
		}
	}
	t.Logf("outcomes %v", outcomes)
	m.mpesaInvariants(invariantOpts{})
}

// Scenario 10: the callback job is retried after a transient failure
// (context cancelled mid-processing) and runs twice at once.
func TestMpesaCallbackJobRetriedIdempotent(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	prod := m.product("RETRY").Product.ID
	pa := m.mpesaPayer(prod)
	var cancelled atomic.Int64
	for i := 0; i < 25; i++ {
		amount := int64(500 + i)
		req := m.request(prod, pa.ID, amount)
		ph := newPhone(0)
		orphan := i%2 == 1
		if orphan {
			m.client.setHook(func(context.Context, mpesa.STKPushRequest, string) error { return transientErr })
		} else {
			m.client.setHook(nil)
		}
		m.mustSend(prod, req.ID, ph)
		co, _ := m.client.last()
		id, err := m.store(store.MpesaCallbackKindStk, stkSuccessBody(co, newMpesaReceipt(), amount, ph))
		if err != nil {
			t.Fatal(err)
		}
		// Attempts cancelled at random points.
		for k := 0; k < 4; k++ {
			ctx, cancel := context.WithTimeout(m.ctx, time.Duration(rand.IntN(4000))*time.Microsecond)
			if err := m.svc.ProcessCallback(ctx, id); err != nil {
				cancelled.Add(1)
			}
			cancel()
		}
		// Then several workers pick up the same job at once.
		errs := storm(8, func(int) error { return m.process(m.ctx, id) })
		requireNoErrors(t, "process", errs)
		if n, sum := m.succeededFor(req.ID); n != 1 || sum != amount {
			t.Fatalf("iteration %d: %d payments totalling %d", i, n, sum)
		}
		assertStatus(t, m.reqState(prod, req.ID), allocation.StatusPaid, money.KES(amount))
	}
	t.Logf("cancelled attempts %d", cancelled.Load())
	m.mpesaInvariants(invariantOpts{})
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
