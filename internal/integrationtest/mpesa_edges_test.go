package integrationtest

// M-Pesa chaos: edge cases found while breaking the scenarios above.

import (
	"context"
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/charge"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// The app cancels a charge whose prompt we had given up on, at the moment
// the customer's late payment arrives (rule 6). Cancel is refused while a
// prompt is live, so the race is with the late success callback. The money
// is recorded once and always pays the charge: either the cancel lost
// (not cancellable) or the charge is cancelled and then succeeded by the
// late money (charge.cancelled followed by charge.succeeded).
func TestMpesaCancelRacesSuccessCallback(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	prod := m.app("CANC").App.ID
	outcomes := map[string]int{}
	for i := 0; i < 30; i++ {
		amount := int64(600 + i)
		req := m.charge(prod, amount)
		ph := newPhone(0)
		p := m.mustSend(prod, req.ID, ph)
		// A live prompt blocks cancel.
		if _, err := m.charges.Cancel(m.ctx, audit.App(prod), prod, req.ID); !errors.Is(err, charge.ErrNotCancellable) {
			t.Fatalf("cancel with a live prompt: %v", err)
		}
		m.client.setQuery(*p.CheckoutRequestID, mpesa.STKQueryResult{State: mpesa.QueryFailed, ResultCode: "1037", ResultDesc: "unreachable"})
		m.advance(2 * time.Minute)
		if r, err := m.svc.Sweep(m.ctx, 10); err != nil || r.Failed != 1 {
			t.Fatalf("sweep: %+v %v", r, err)
		}
		body := stkSuccessBody(*p.CheckoutRequestID, newMpesaReceipt(), amount, ph)
		var cancelErr error
		errs := storm(2, func(g int) error {
			if g == 0 {
				_, err := m.deliver(store.MpesaCallbackKindStk, body)
				return err
			}
			time.Sleep(time.Duration(rand.IntN(5000*(i%2)+1)) * time.Microsecond)
			_, cancelErr = m.charges.Cancel(m.ctx, audit.App(prod), prod, req.ID)
			if cancelErr != nil && !errors.Is(cancelErr, charge.ErrNotCancellable) {
				return cancelErr
			}
			return nil
		})
		requireNoErrors(t, "race", errs)
		m.assertCharge(req.ID, store.ChargeStatusSucceeded)
		if n, sum := m.succeededFor(req.ID); n != 1 || sum != amount {
			t.Fatalf("iteration %d: %d payments totalling %d", i, n, sum)
		}
		cancelled := m.eventsFor(req.ID, events.ChargeCancelled)
		if cancelErr == nil {
			outcomes["cancel first: late money still succeeds it"]++
			if cancelled != 1 {
				t.Fatalf("iteration %d: cancel won but %d charge.cancelled events", i, cancelled)
			}
		} else {
			outcomes["callback first: cancel refused"]++
			if cancelled != 0 {
				t.Fatalf("iteration %d: cancel refused but charge.cancelled emitted", i)
			}
		}
		if c := m.eventsFor(req.ID, events.ChargeSucceeded); c != 1 {
			t.Fatalf("iteration %d: charge.succeeded events %d", i, c)
		}
		if got := m.push(p.ID); got.Status != store.StkPushStatusSucceeded {
			t.Fatalf("iteration %d: push %s", i, got.Status)
		}
	}
	t.Logf("outcomes %v", outcomes)
	m.mpesaInvariants()
}

// A customer retries after Daraja timed out: push A (Daraja's answer lost,
// later failed "not_sent" by the sweeper) and push B for the SAME request,
// phone and amount (answer lost too). B's success callback then has two
// orphan candidates (A failed, B initiating) for the same charge: the
// single live one (B) wins and the money pays the charge, never unmatched.
func TestMpesaOrphanAfterRetryGoesToSuspense(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	m.client.setHook(func(context.Context, mpesa.STKPushRequest, string) error { return transientErr })
	prod := m.app("ORPH").App.ID
	req := m.charge(prod, 800)
	ph := newPhone(5)

	a := m.mustSend(prod, req.ID, ph)
	m.advance(3 * time.Minute)
	if r, err := m.svc.Sweep(m.ctx, 10); err != nil || r.Failed != 1 {
		t.Fatalf("sweep A: %+v %v", r, err)
	}
	b := m.mustSend(prod, req.ID, ph)
	coB, _ := m.client.last()
	m.mustDeliver(store.MpesaCallbackKindStk, stkSuccessBody(coB, newMpesaReceipt(), 800, ph))

	unmatched := m.count(`SELECT count(*) FROM transactions WHERE app_id IS NULL`)
	gotB := m.push(b.ID)
	t.Logf("push A %s, push B %s, unmatched %d, request %s", m.push(a.ID).Status, gotB.Status, unmatched, m.chargeOf(req.ID).Status)
	m.mpesaInvariants()
	if unmatched != 0 || gotB.Status != store.StkPushStatusSucceeded {
		t.Fatalf("B's receipt recorded unmatched (%d rows); push B %s; charge %s, want succeeded",
			unmatched, gotB.Status, m.chargeOf(req.ID).Status)
	}
	m.assertCharge(req.ID, store.ChargeStatusSucceeded)
}

// Staff confirm a needs_review payment with the receipt that is (unknown to
// them) also arriving as the late STK callback AND as a C2B confirmation,
// all at once. Recorded once.
func TestMpesaResolveCallbackAndC2BSameReceipt(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	prod := m.app("TRI").App.ID
	for i := 0; i < 20; i++ {
		amount := int64(900 + i)
		req := m.charge(prod, amount)
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
				_, err := m.deliver(store.MpesaCallbackKindC2bConfirmation, c2bBody(receipt, amount, req.AccountNumber))
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
		m.assertCharge(req.ID, store.ChargeStatusSucceeded)
		if n, sum := m.succeededFor(req.ID); n != 1 || sum != amount {
			t.Fatalf("iteration %d: %d payments totalling %d (money counted twice?)", i, n, sum)
		}
	}
	m.mpesaInvariants()
}
