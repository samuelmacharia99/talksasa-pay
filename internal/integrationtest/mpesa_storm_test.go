//go:build gatewaycore

// TODO(gateway-core): M-Pesa chaos/invariant suite, still written against
// payers and payment requests. Compiled only with -tags gatewaycore until
// the money core ports it to charges.

package integrationtest

// M-Pesa chaos: a full storm through the real Daraja client and the HTTP
// fake (mpesatest), with callbacks delivered over HTTP, stored, duplicated
// and processed by a River-like worker pool, sweepers running throughout,
// and paybill (C2B) payments to the same payers.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/mpesa/mpesatest"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Scenario 8: 200 pushes across 20 concurrent request lanes (10 payers),
// random fake outcomes, random duplicate callback deliveries, three
// sweepers, C2B payments to the same payers (and to an unknown account),
// then staff resolve every needs_review payment concurrently. Every
// receipt Safaricom issued is recorded exactly once, and nothing else is.
func TestMpesaStorm(t *testing.T) {
	if testing.Short() {
		t.Skip("storm")
	}
	const (
		lanes       = 20
		payersN     = 10
		pushTarget  = 200
		workers     = 8
		cbDelay     = 30 * time.Millisecond
		pollTimeout = 6 * time.Second
	)
	fake := mpesatest.New(t, mpesatest.Options{CallbackDelay: cbDelay})

	var (
		m          *mpesaEnv
		jobs       = make(chan uuid.UUID, 10000)
		cbStored   atomic.Int64
		cbDupes    atomic.Int64
		cbStoreErr atomic.Int64
	)
	recv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		kind := store.MpesaCallbackKindStk
		switch {
		case strings.HasSuffix(r.URL.Path, "/stk"):
		case strings.HasSuffix(r.URL.Path, "/c2b/confirmation"):
			kind = store.MpesaCallbackKindC2bConfirmation
		default:
			http.NotFound(w, r)
			return
		}
		copies := 1
		if rand.IntN(100) < 15 {
			copies = 2 // Safaricom retried: the same body arrives again
			cbDupes.Add(1)
		}
		for c := 0; c < copies; c++ {
			id, err := m.store(kind, string(body))
			if err != nil {
				cbStoreErr.Add(1)
				http.Error(w, "store failed", http.StatusInternalServerError)
				return
			}
			cbStored.Add(1)
			jobs <- id
		}
		_, _ = io.WriteString(w, `{"ResultCode":0,"ResultDesc":"Accepted"}`)
	}))
	defer recv.Close()

	client, err := mpesa.NewClient(mpesa.ClientOptions{
		Env: config.MpesaSandbox, BaseURL: fake.URL, ShortCode: mpesaShortCode,
		ConsumerKey: config.NewSecret("key"), ConsumerSecret: config.NewSecret("secret"), Passkey: config.NewSecret("passkey"),
		CallbackToken:   config.NewSecret(strings.Repeat("t", 40)),
		CallbackBaseURL: recv.URL + "/", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	m = newMpesaEnv(t, client, func(o *stk.Options) {
		o.QueryAfter = 150 * time.Millisecond
		o.GiveUpAfter = 2 * time.Second
		o.InitiatingTimeout = time.Second
		o.SweepLease = 300 * time.Millisecond
	})
	_, conf, val := client.CallbackURLs()
	if err := client.RegisterC2BURLs(m.ctx, mpesa.RegisterURLsRequest{ShortCode: mpesaShortCode, ConfirmationURL: conf, ValidationURL: val, ResponseType: "Completed"}); err != nil {
		t.Fatal(err)
	}

	prod := m.app("STRM").App.ID
	payers := make([]payer.Payer, payersN)
	for i := range payers {
		payers[i] = m.mpesaPayer(prod)
	}

	// Callback workers (River stand-in: retries on error).
	var (
		wwg       sync.WaitGroup
		processed atomic.Int64
		procErrs  atomic.Int64
	)
	for w := 0; w < workers; w++ {
		wwg.Add(1)
		go func() {
			defer wwg.Done()
			for id := range jobs {
				if err := m.process(m.ctx, id); err != nil {
					procErrs.Add(1)
					t.Errorf("process: %v", err)
				}
				processed.Add(1)
			}
		}()
	}

	stop := make(chan struct{})
	var bg sync.WaitGroup
	var sweepRes struct {
		sync.Mutex
		stk.SweepResult
		errs int
	}
	for s := 0; s < 3; s++ {
		bg.Add(1)
		go func() {
			defer bg.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Duration(10+rand.IntN(30)) * time.Millisecond):
				}
				r, err := m.svc.Sweep(m.ctx, 200)
				sweepRes.Lock()
				sweepRes.Queried += r.Queried
				sweepRes.Failed += r.Failed
				sweepRes.NeedsReview += r.NeedsReview
				sweepRes.Rescheduled += r.Rescheduled
				if err != nil {
					sweepRes.errs++
				}
				sweepRes.Unlock()
			}
		}()
	}

	// Paybill payments to the same payers (and some to nobody).
	var c2bMu sync.Mutex
	c2bReceipts := map[string]int64{}
	bg.Add(1)
	go func() {
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(20+rand.IntN(40)) * time.Millisecond):
			}
			ref := payers[rand.IntN(payersN)].AccountNumber
			if rand.IntN(10) == 0 {
				ref = "NOBODY-1"
			}
			amount := int64(10 + rand.IntN(400))
			rc, err := fake.SimulateC2B(m.ctx, ref, amount, newPhone(0))
			if err != nil {
				t.Errorf("c2b: %v", err)
				continue
			}
			c2bMu.Lock()
			c2bReceipts[rc] = amount
			c2bMu.Unlock()
		}
	}()

	// Behaviour weights by phone last digit (0 success ... 6 duplicate).
	digits := []int{0, 0, 0, 1, 1, 1, 2, 3, 4, 4, 5, 5, 6, 6}
	var (
		pushes, inProgress, rejected, notPayable atomic.Int64
		laneErr                                  = make(chan error, lanes)
		reqSeq                                   atomic.Int64
	)
	start := time.Now()
	var lwg sync.WaitGroup
	for l := 0; l < lanes; l++ {
		lwg.Add(1)
		go func() {
			defer lwg.Done()
			pa := payers[l%payersN]
			var reqID uuid.UUID
			for pushes.Load() < pushTarget {
				if reqID == uuid.Nil {
					n := reqSeq.Add(1)
					c, err := m.createReq(prod, pa.ID, fmt.Sprintf("S-%d", n), fmt.Sprintf("storm-%d", n), int64(100+rand.IntN(5000)), nil)
					if err != nil {
						laneErr <- fmt.Errorf("create request: %w", err)
						return
					}
					reqID = c.Request.ID
				}
				sp := stk.SendParams{AppID: prod, RequestID: reqID, Phone: newPhone(digits[rand.IntN(len(digits))]), Source: stk.SourcePayPage}
				if rand.IntN(2) == 0 {
					sp.Source, sp.IdempotencyKey = stk.SourceApp, uuid.NewString()
				}
				p, _, err := m.svc.Send(m.ctx, audit.App(prod), sp)
				switch {
				case err == nil:
					pushes.Add(1)
				case errors.Is(err, stk.ErrDaraja):
					pushes.Add(1)
					rejected.Add(1)
					continue
				case isInProgress(err):
					inProgress.Add(1)
					time.Sleep(20 * time.Millisecond)
					continue
				case errors.Is(err, stk.ErrNotPayable):
					notPayable.Add(1)
					reqID = uuid.Nil
					continue
				default:
					laneErr <- fmt.Errorf("send: %w", err)
					return
				}
				if rand.IntN(5) == 0 {
					continue // don't wait: the next Send must hit the one-live-push guard
				}
				deadline := time.Now().Add(pollTimeout)
				for time.Now().Before(deadline) {
					cur, err := store.New(m.db.App).GetSTKPushInternal(m.ctx, p.ID)
					if err != nil {
						laneErr <- err
						return
					}
					if cur.Status != store.StkPushStatusInitiating && cur.Status != store.StkPushStatusSent {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		}()
	}
	lwg.Wait()
	elapsed := time.Since(start)
	close(laneErr)
	for err := range laneErr {
		t.Fatal(err)
	}

	// Let the fake deliver what is still due, then settle every live push.
	time.Sleep(10 * cbDelay)
	close(stop)
	bg.Wait()
	fake.Close() // no more callbacks
	m.advance(time.Hour)
	for i := 0; ; i++ {
		if _, err := m.svc.Sweep(m.ctx, 1000); err != nil {
			t.Logf("final sweep: %v", err)
		}
		if m.count(`SELECT count(*) FROM stk_pushes WHERE status IN ('initiating', 'sent')`) == 0 {
			break
		}
		if i == 50 {
			t.Fatal("live pushes left after final sweeps")
		}
		time.Sleep(20 * time.Millisecond)
	}
	recv.Close()
	close(jobs)
	wwg.Wait()

	// Staff resolve every needs_review payment at once: confirm with the
	// receipt Safaricom has (the fake's portal), else fail.
	type review struct {
		txn     uuid.UUID
		receipt string
	}
	var reviews []review
	rows, err := m.db.Owner.Query(m.ctx, `SELECT t.id, p.checkout_request_id, t.failure_code FROM transactions t
		JOIN stk_pushes p ON p.transaction_id = t.id WHERE t.status = 'needs_review'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		var co, code *string
		if err := rows.Scan(&id, &co, &code); err != nil {
			t.Fatal(err)
		}
		rv := review{txn: id}
		if co != nil {
			if fp, ok := fake.Push(*co); ok && fp.Receipt != "" && deref(code) != "amount_mismatch" {
				rv.receipt = fp.Receipt
			}
		}
		reviews = append(reviews, rv)
	}
	rows.Close()
	resolved := map[bool]int{}
	requireNoErrors(t, "resolve", storm(len(reviews), func(i int) error {
		rv := reviews[i]
		_, err := m.svc.Resolve(m.ctx, mpesaStaff(), stk.ResolveParams{TransactionID: rv.txn, Confirm: rv.receipt != "", Receipt: rv.receipt, Reason: "checked the M-Pesa portal"})
		return err
	}))
	for _, rv := range reviews {
		resolved[rv.receipt != ""]++
	}

	// Conservation: the set of receipts in the ledger is exactly the set
	// Safaricom issued and told us about (callback delivered, staff
	// confirmed, or paybill confirmation), each with its amount.
	want := map[string]int64{}
	for _, fp := range fake.Pushes() {
		if fp.Receipt != "" && fp.CallbacksSent > 0 {
			want[fp.Receipt] = fp.Amount
		}
	}
	for _, rv := range reviews {
		if rv.receipt != "" {
			fp, _ := fake.Push(m.checkoutOf(rv.txn))
			want[rv.receipt] = fp.Amount
		}
	}
	for rc, a := range c2bReceipts {
		want[rc] = a
	}
	got := map[string]int64{}
	rows, err = m.db.Owner.Query(m.ctx, `SELECT provider_transaction_id, amount FROM transactions WHERE provider_transaction_id IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var rc string
		var a int64
		if err := rows.Scan(&rc, &a); err != nil {
			t.Fatal(err)
		}
		if _, dup := got[rc]; dup {
			t.Fatalf("receipt %s recorded twice", rc)
		}
		got[rc] = a
	}
	rows.Close()
	for rc, a := range want {
		if g, ok := got[rc]; !ok {
			t.Errorf("receipt %s (KES %d) issued by Safaricom but not recorded", rc, a)
		} else if g != a {
			t.Errorf("receipt %s recorded as KES %d, Safaricom said %d", rc, g, a)
		}
	}
	for rc := range got {
		if _, ok := want[rc]; !ok {
			t.Errorf("receipt %s recorded but never issued", rc)
		}
	}

	m.mpesaInvariants(invariantOpts{})

	byStatus := map[string]int64{}
	srows, err := m.db.Owner.Query(m.ctx, `SELECT status::text, count(*) FROM stk_pushes GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for srows.Next() {
		var s string
		var n int64
		if err := srows.Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		byStatus[s] = n
	}
	srows.Close()
	paidReqs := m.count(`SELECT count(*) FROM payment_requests WHERE status = 'paid'`)
	t.Logf("storm: %d pushes (%d rejected) in %s = %.1f pushes/s; in-progress refusals %d, not-payable %d",
		pushes.Load(), rejected.Load(), elapsed.Round(time.Millisecond), float64(pushes.Load())/elapsed.Seconds(), inProgress.Load(), notPayable.Load())
	t.Logf("callbacks: stored %d (%d duplicated deliveries, %d store errors), processed %d (%.0f/s), retried attempts %d, failures %d",
		cbStored.Load(), cbDupes.Load(), cbStoreErr.Load(), processed.Load(), float64(processed.Load())/elapsed.Seconds(), m.retries.Load(), procErrs.Load())
	t.Logf("push states %v; requests paid %d; c2b payments %d; sweeps %+v; staff resolved confirm=%d fail=%d; receipts recorded %d",
		byStatus, paidReqs, len(c2bReceipts), sweepRes.SweepResult, resolved[true], resolved[false], len(got))
	if n := m.count(`SELECT count(*) FROM payment_requests WHERE status = 'paid'`); n == 0 {
		t.Fatal("storm paid nothing")
	}
}

// checkoutOf returns the checkout id of the push owning a transaction.
func (m *mpesaEnv) checkoutOf(txnID uuid.UUID) string {
	m.t.Helper()
	var co *string
	if err := m.db.Owner.QueryRow(context.Background(), `SELECT checkout_request_id FROM stk_pushes WHERE transaction_id = $1`, txnID).Scan(&co); err != nil {
		m.t.Fatal(err)
	}
	return deref(co)
}
