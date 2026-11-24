package integrationtest

// M-Pesa chaos: Send under concurrency and crashes.

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Scenario 5: Daraja accepted the push but we never saved its answer (a
// transient error after acceptance stands in for a crash between Tx A and
// Tx B). (a) the success callback arrives: matched as an orphan, money
// recorded once on the TARGET request (not the payer's oldest); (b) no
// callback: the sweeper fails it after InitiatingTimeout; (c) a late
// callback after (b): recorded once as a rescue; (d) callback duplicates
// racing sweepers that are failing the same orphan push.
func TestMpesaCrashBetweenTxAAndDaraja(t *testing.T) {
	m := newMpesaEnv(t, nil, func(o *stk.Options) { o.InitiatingTimeout = 2 * time.Minute })
	m.client.setHook(func(context.Context, mpesa.STKPushRequest, string) error { return transientErr })
	prod := m.product("CRASH").Product.ID

	setup := func(amount int64) (oldest, target uuid.UUID, p stk.Push, co, ph string) {
		pa := m.mpesaPayer(prod)
		old := m.request(prod, pa.ID, amount+1)
		tgt := m.request(prod, pa.ID, amount)
		ph = newPhone(5)
		p = m.mustSend(prod, tgt.ID, ph)
		if p.Status != store.StkPushStatusInitiating {
			t.Fatalf("setup: push %s", p.Status)
		}
		co, _ = m.client.last()
		return old.ID, tgt.ID, p, co, ph
	}
	assertTarget := func(what string, oldest, target uuid.UUID, amount int64) {
		t.Helper()
		if n, sum := m.succeededFor(target); n != 1 || sum != amount {
			t.Fatalf("%s: %d payments totalling %d on the target", what, n, sum)
		}
		assertStatus(t, m.reqState(prod, target), allocation.StatusPaid, money.KES(amount))
		assertStatus(t, m.reqState(prod, oldest), allocation.StatusOpen, 0)
	}

	t.Run("callback arrives", func(t *testing.T) {
		old, tgt, p, co, ph := setup(700)
		m.mustDeliver(store.MpesaCallbackKindStk, stkSuccessBody(co, newMpesaReceipt(), 700, ph))
		assertTarget("orphan callback", old, tgt, 700)
		if got := m.push(p.ID); got.Status != store.StkPushStatusSucceeded {
			t.Fatalf("push %s", got.Status)
		}
	})

	var rescue struct {
		old, tgt uuid.UUID
		p        stk.Push
		co, ph   string
	}
	t.Run("no callback: sweeper fails it", func(t *testing.T) {
		old, tgt, p, co, ph := setup(710)
		rescue.old, rescue.tgt, rescue.p, rescue.co, rescue.ph = old, tgt, p, co, ph
		if r, err := m.svc.Sweep(m.ctx, 50); err != nil || r.Failed != 0 {
			t.Fatalf("early sweep failed it: %+v %v", r, err)
		}
		m.advance(3 * time.Minute)
		if r, err := m.svc.Sweep(m.ctx, 50); err != nil || r.Failed != 1 {
			t.Fatalf("sweep: %+v %v", r, err)
		}
		got, txn := m.push(p.ID), m.txn(p.TransactionID)
		if got.Status != store.StkPushStatusFailed || txn.Status != store.TxnStatusFailed || deref(txn.FailureCode) != "not_sent" {
			t.Fatalf("push %s txn %s code %s", got.Status, txn.Status, deref(txn.FailureCode))
		}
		if c := m.eventsFor(p.ID, events.STKPushFailed); c != 1 {
			t.Fatalf("failed events %d", c)
		}
		assertStatus(t, m.reqState(prod, tgt), allocation.StatusOpen, 0)
	})

	t.Run("late callback after give-up", func(t *testing.T) {
		if rescue.p.ID == uuid.Nil {
			t.Skip("previous subtest failed")
		}
		body := stkSuccessBody(rescue.co, newMpesaReceipt(), 710, rescue.ph)
		requireNoErrors(t, "late", storm(10, func(int) error { _, err := m.deliver(store.MpesaCallbackKindStk, body); return err }))
		assertTarget("rescue", rescue.old, rescue.tgt, 710)
		if got := m.push(rescue.p.ID); got.Status != store.StkPushStatusSucceeded {
			t.Fatalf("push %s", got.Status)
		}
	})

	t.Run("callbacks race sweepers failing the orphan", func(t *testing.T) {
		outcomes := map[string]int{}
		for i := 0; i < 20; i++ {
			amount := int64(720 + i)
			old, tgt, p, co, ph := setup(amount)
			m.advance(3 * time.Minute) // due for the initiating timeout
			body := stkSuccessBody(co, newMpesaReceipt(), amount, ph)
			errs := storm(8, func(g int) error {
				if g < 3 {
					time.Sleep(time.Duration(rand.IntN(6000*(i%2)+1)) * time.Microsecond)
					_, err := m.svc.Sweep(m.ctx, 50)
					return err
				}
				_, err := m.deliver(store.MpesaCallbackKindStk, body)
				return err
			})
			requireNoErrors(t, "race", errs)
			assertTarget("race", old, tgt, amount)
			got := m.push(p.ID)
			if got.Status != store.StkPushStatusSucceeded {
				t.Fatalf("iteration %d: push %s", i, got.Status)
			}
			if m.txn(p.TransactionID).Status == store.TxnStatusFailed {
				outcomes["sweeper first (rescue)"]++
			} else {
				outcomes["callback first"]++
			}
		}
		t.Logf("outcomes %v", outcomes)
	})
	m.mpesaInvariants(invariantOpts{allowCreditWithPayable: false})
}

// Scenario 6: many Sends for the same request at once. Different keys and
// the pay page: exactly one live push, the rest InProgressError; the same
// key 50 times: one push, the rest replays; Daraja is asked once.
func TestMpesaConcurrentSends(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	prod := m.product("SEND").Product.ID
	pa := m.mpesaPayer(prod)
	actor := audit.Product(prod)
	const n = 50

	t.Run("different keys and pay page", func(t *testing.T) {
		req := m.request(prod, pa.ID, 900)
		before := m.client.pushCount()
		var won atomic.Int64
		start := time.Now()
		errs := storm(n, func(i int) error {
			sp := stk.SendParams{ProductID: prod, RequestID: req.ID, Phone: newPhone(0), Source: stk.SourcePayPage}
			if i%2 == 0 {
				sp.Source, sp.IdempotencyKey = stk.SourceProduct, uuid.NewString()
			}
			_, replayed, err := m.svc.Send(m.ctx, actor, sp)
			switch {
			case err == nil && !replayed:
				won.Add(1)
				return nil
			case isInProgress(err):
				return nil
			case err == nil:
				return errors.New("replayed with a fresh key")
			}
			return err
		})
		requireNoErrors(t, "send", errs)
		t.Logf("%s", opsPerSec(n, time.Since(start)))
		if won.Load() != 1 {
			t.Fatalf("%d sends won", won.Load())
		}
		if c := m.count(`SELECT count(*) FROM stk_pushes WHERE payment_request_id = $1`, req.ID); c != 1 {
			t.Fatalf("%d pushes", c)
		}
		if c := m.count(`SELECT count(*) FROM transactions WHERE payment_request_id = $1`, req.ID); c != 1 {
			t.Fatalf("%d transactions", c)
		}
		if got := m.client.pushCount() - before; got != 1 {
			t.Fatalf("Daraja asked %d times", got)
		}
	})

	t.Run("same key", func(t *testing.T) {
		req := m.request(prod, pa.ID, 950)
		key, ph := uuid.NewString(), newPhone(0)
		before := m.client.pushCount()
		ids := make([]uuid.UUID, n)
		var fresh atomic.Int64
		errs := storm(n, func(i int) error {
			p, replayed, err := m.svc.Send(m.ctx, actor, stk.SendParams{ProductID: prod, RequestID: req.ID, Phone: ph, Source: stk.SourceProduct, IdempotencyKey: key})
			if err != nil {
				return err
			}
			if !replayed {
				fresh.Add(1)
			}
			ids[i] = p.ID
			return nil
		})
		requireNoErrors(t, "send", errs)
		if fresh.Load() != 1 {
			t.Fatalf("%d non-replayed sends", fresh.Load())
		}
		for _, id := range ids {
			if id != ids[0] {
				t.Fatalf("different push ids returned: %s vs %s", id, ids[0])
			}
		}
		if got := m.client.pushCount() - before; got != 1 {
			t.Fatalf("Daraja asked %d times", got)
		}
	})

	t.Run("many requests at once", func(t *testing.T) {
		const reqs = 10
		var ids [reqs]uuid.UUID
		for i := range ids {
			ids[i] = m.request(prod, pa.ID, int64(1000+i)).ID
		}
		var won [reqs]atomic.Int64
		errs := storm(reqs*10, func(i int) error {
			_, replayed, err := m.svc.Send(m.ctx, actor, stk.SendParams{ProductID: prod, RequestID: ids[i%reqs], Phone: newPhone(0), Source: stk.SourcePayPage})
			if err == nil && !replayed {
				won[i%reqs].Add(1)
				return nil
			}
			if isInProgress(err) {
				return nil
			}
			return err
		})
		requireNoErrors(t, "send", errs)
		for i := range won {
			if won[i].Load() != 1 {
				t.Fatalf("request %d: %d sends won", i, won[i].Load())
			}
		}
	})
	// Settle every live push (customer cancelled) so the invariants see
	// final states too.
	rows, err := m.db.Owner.Query(m.ctx, `SELECT checkout_request_id FROM stk_pushes WHERE status = 'sent'`)
	if err != nil {
		t.Fatal(err)
	}
	var cos []string
	for rows.Next() {
		var co string
		if err := rows.Scan(&co); err != nil {
			t.Fatal(err)
		}
		cos = append(cos, co)
	}
	rows.Close()
	requireNoErrors(t, "cancel", storm(len(cos), func(i int) error {
		_, err := m.deliver(store.MpesaCallbackKindStk, stkFailureBody(cos[i], "1032", "Request cancelled by user"))
		return err
	}))
	m.mpesaInvariants(invariantOpts{})
}
