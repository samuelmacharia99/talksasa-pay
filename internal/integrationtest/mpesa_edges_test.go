package integrationtest

// M-Pesa chaos: edge cases found while breaking the scenarios above.

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// The product cancels a request while its STK push is live and the
// customer pays at the same moment. The money is recorded once: either the
// request is paid (cancel refused) or it is cancelled and the money becomes
// credit; a cancelled request never receives money.
func TestMpesaCancelRacesSuccessCallback(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	prod := m.product("CANC").Product.ID
	outcomes := map[string]int{}
	for i := 0; i < 30; i++ {
		pa := m.mpesaPayer(prod)
		amount := int64(600 + i)
		req := m.request(prod, pa.ID, amount)
		ph := newPhone(0)
		p := m.mustSend(prod, req.ID, ph)
		body := stkSuccessBody(*p.CheckoutRequestID, newMpesaReceipt(), amount, ph)
		var cancelErr error
		errs := storm(2, func(g int) error {
			if g == 0 {
				_, err := m.deliver(store.MpesaCallbackKindStk, body)
				return err
			}
			time.Sleep(time.Duration(rand.IntN(5000*(i%2)+1)) * time.Microsecond)
			_, cancelErr = m.requests.Cancel(m.ctx, audit.Product(prod), prod, req.ID)
			if cancelErr != nil && !errors.Is(cancelErr, paymentrequest.ErrNotCancellable) {
				return cancelErr
			}
			return nil
		})
		requireNoErrors(t, "race", errs)
		r := m.reqState(prod, req.ID)
		if cancelErr == nil {
			outcomes["cancel first: money to credit"]++
			assertStatus(t, r, allocation.StatusCancelled, 0)
			if c := m.credit(prod, pa.ID); c != money.KES(amount) {
				t.Fatalf("iteration %d: credit %d, want %d", i, c, amount)
			}
		} else {
			outcomes["callback first: paid"]++
			assertStatus(t, r, allocation.StatusPaid, money.KES(amount))
		}
		if got := m.push(p.ID); got.Status != store.StkPushStatusSucceeded {
			t.Fatalf("iteration %d: push %s", i, got.Status)
		}
	}
	t.Logf("outcomes %v", outcomes)
	m.mpesaInvariants(invariantOpts{})
}

// A customer retries after Daraja timed out: push A (Daraja's answer lost,
// later failed "not_sent" by the sweeper) and push B for the SAME request,
// phone and amount (answer lost too). B's success callback then has two
// orphan candidates (A failed, B initiating), so it is recorded unmatched
// (suspense) instead of paying the request, and the sweeper later fails B.
// Money is not lost or doubled (it sits in suspense), but the request the
// customer paid stays open and the customer may pay again.
func TestMpesaOrphanAfterRetryGoesToSuspense(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	m.client.setHook(func(context.Context, mpesa.STKPushRequest, string) error { return transientErr })
	prod := m.product("ORPH").Product.ID
	pa := m.mpesaPayer(prod)
	req := m.request(prod, pa.ID, 800)
	ph := newPhone(5)

	a := m.mustSend(prod, req.ID, ph)
	m.advance(3 * time.Minute)
	if r, err := m.svc.Sweep(m.ctx, 10); err != nil || r.Failed != 1 {
		t.Fatalf("sweep A: %+v %v", r, err)
	}
	b := m.mustSend(prod, req.ID, ph)
	coB, _ := m.client.last()
	m.mustDeliver(store.MpesaCallbackKindStk, stkSuccessBody(coB, newMpesaReceipt(), 800, ph))

	unmatched := m.count(`SELECT count(*) FROM transactions WHERE product_id IS NULL`)
	gotB := m.push(b.ID)
	t.Logf("push A %s, push B %s, unmatched %d, request %s", m.push(a.ID).Status, gotB.Status, unmatched, m.reqState(prod, req.ID).Status)
	m.mpesaInvariants(invariantOpts{})
	if unmatched != 0 || gotB.Status != store.StkPushStatusSucceeded {
		mpesaBugGate(t, "orphan success callback goes to suspense when an earlier not_sent push for the same request/phone/amount exists")
		t.Fatalf("B's receipt recorded unmatched (%d rows in suspense); push B %s; request %s, want paid",
			unmatched, gotB.Status, m.reqState(prod, req.ID).Status)
	}
	assertStatus(t, m.reqState(prod, req.ID), allocation.StatusPaid, 800)
}

// Staff confirm a needs_review payment with the receipt that is (unknown to
// them) also arriving as the late STK callback AND as a C2B confirmation,
// all at once. Recorded once.
func TestMpesaResolveCallbackAndC2BSameReceipt(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	prod := m.product("TRI").Product.ID
	for i := 0; i < 20; i++ {
		pa := m.mpesaPayer(prod)
		amount := int64(900 + i)
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
		errs := storm(3, func(g int) error {
			switch g {
			case 0:
				_, err := m.deliver(store.MpesaCallbackKindStk, stkSuccessBody(co, receipt, amount, ph))
				return err
			case 1:
				_, err := m.deliver(store.MpesaCallbackKindC2bConfirmation, c2bBody(receipt, amount, pa.AccountNumber))
				return err
			}
			_, err := m.svc.Resolve(m.ctx, mpesaStaff(), stk.ResolveParams{TransactionID: p.TransactionID, Confirm: true, Receipt: receipt, Reason: "portal"})
			if err != nil && !errors.Is(err, stk.ErrNotResolvable) && !errors.Is(err, stk.ErrReceiptTaken) {
				return err
			}
			return nil
		})
		requireNoErrors(t, "race", errs)
		if c := m.receiptRows(receipt); c != 1 {
			t.Fatalf("iteration %d: receipt on %d rows", i, c)
		}
		assertStatus(t, m.reqState(prod, req.ID), allocation.StatusPaid, money.KES(amount))
		if c := m.credit(prod, pa.ID); c != 0 {
			t.Fatalf("iteration %d: credit %d (money counted twice?)", i, c)
		}
	}
	m.mpesaInvariants(invariantOpts{})
}
