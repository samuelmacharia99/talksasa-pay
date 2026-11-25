//go:build gatewaycore

// TODO(gateway-core): these tests still use payers and payment requests.
// They are compiled only with -tags gatewaycore until the money core ports
// them to charges (helpers_test.go already seeds apps and charges).

package stk

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/store"
)

const custPhone = "254712345678"

func TestSendAndSuccessCallback(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 1500)

	p := e.send(prod, r.ID, "0712 345 678")
	if p.Status != store.StkPushStatusSent || p.CheckoutRequestID == nil || p.Amount != 1500 || p.Phone != custPhone {
		t.Fatalf("push after send: %+v", p)
	}
	req, co := e.fake.lastPush()
	if req.AccountReference != pa.AccountNumber || req.TransactionDesc != r.AppReference || req.Amount != 1500 || req.Phone != custPhone {
		t.Fatalf("daraja request: %+v", req)
	}
	if *p.CheckoutRequestID != co {
		t.Fatalf("checkout id %s, want %s", *p.CheckoutRequestID, co)
	}
	pending := e.txn(p.TransactionID)
	if pending.Status != store.TxnStatusPending || pending.ProviderRequestID == nil || *pending.ProviderRequestID != co ||
		*pending.BillRef != pa.AccountNumber || pending.Channel != store.TxnChannelMpesaStk {
		t.Fatalf("pending txn: %+v", pending)
	}
	if e.events(p.ID, events.STKPushSent) != 1 {
		t.Fatal("stk_push.sent not emitted")
	}

	// Get / LatestForRequest / ListForRequest.
	if got, err := e.svc.Get(e.ctx, prod, p.ID); err != nil || got.ID != p.ID {
		t.Fatalf("Get: %v", err)
	}
	if _, err := e.svc.Get(e.ctx, e.app("OTHR"), p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get other app: %v", err)
	}
	if l, err := e.svc.LatestForRequest(e.ctx, r.ID, time.Now().Add(-time.Minute)); err != nil || l == nil || l.ID != p.ID {
		t.Fatalf("LatestForRequest: %v %v", l, err)
	}
	if l, err := e.svc.LatestForRequest(e.ctx, r.ID, time.Now().Add(time.Minute)); err != nil || l != nil {
		t.Fatalf("LatestForRequest future: %v %v", l, err)
	}
	if ls, err := e.svc.ListForRequest(e.ctx, r.ID, 10); err != nil || len(ls) != 1 {
		t.Fatalf("ListForRequest: %v %v", ls, err)
	}

	receipt := newReceipt()
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(co, receipt, 1500, custPhone))
	if cb.ProcessedAt == nil || cb.Error != nil || cb.TransactionID == nil || *cb.TransactionID != p.TransactionID {
		t.Fatalf("callback row: %+v", cb)
	}
	done := e.txn(p.TransactionID)
	if done.Status != store.TxnStatusSucceeded || *done.ProviderTransactionID != receipt || done.OccurredAt == nil || done.RawPayload == nil {
		t.Fatalf("txn after callback: %+v", done)
	}
	if got := e.req(prod, r.ID); got.Status != allocation.StatusPaid || got.AmountPaid != 1500 {
		t.Fatalf("request: %+v", got)
	}
	if got := e.push(p.ID); got.Status != store.StkPushStatusSucceeded || got.CompletedAt == nil || *got.ResultCode != "0" {
		t.Fatalf("push: %+v", got)
	}
	if e.events(p.ID, events.STKPushSucceeded) != 1 || e.events(r.ID, events.PaymentRequestPaid) != 1 ||
		e.events(p.TransactionID, events.TransactionAllocated) != 1 {
		t.Fatal("events missing")
	}

	// Duplicate delivery (new inbox row) and re-processing the same row.
	dup := e.deliver(store.MpesaCallbackKindStk, stkSuccess(co, receipt, 1500, custPhone))
	if dup.Error != nil || dup.TransactionID == nil || *dup.TransactionID != p.TransactionID {
		t.Fatalf("duplicate callback: %+v", dup)
	}
	if err := e.svc.ProcessCallback(e.ctx, cb.ID); err != nil {
		t.Fatal(err)
	}
	if e.count(`SELECT count(*) FROM transactions WHERE provider_transaction_id = $1`, receipt) != 1 ||
		e.events(p.ID, events.STKPushSucceeded) != 1 || e.events(r.ID, events.PaymentRequestPaid) != 1 {
		t.Fatal("duplicate changed something")
	}
	// Paid: no more pushes.
	_, _, err := e.svc.Send(e.ctx, audit.App(prod), SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourcePayPage})
	mustErr(t, err, ErrNotPayable)
	e.checkInvariants()
	e.assertNoLeak()
}

func TestSendAsksForOutstandingBalance(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 1000)
	// Part-pay by C2B.
	cb := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 400, pa.AccountNumber, testShortCode))
	if cb.Error != nil {
		t.Fatalf("c2b: %v", *cb.Error)
	}
	p, _, err := e.svc.Send(e.ctx, audit.App(prod), SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourcePayPage})
	if err != nil {
		t.Fatal(err)
	}
	if p.Amount != 600 || p.Source != string(SourcePayPage) || p.IdempotencyKey != nil {
		t.Fatalf("push: %+v", p)
	}
	e.checkInvariants()
}

func TestSendValidation(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 100)
	act := audit.App(prod)
	sp := func(mut func(*SendParams)) error {
		p := SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourceApp, IdempotencyKey: uuid.NewString()}
		mut(&p)
		_, _, err := e.svc.Send(e.ctx, act, p)
		return err
	}
	mustErr(t, sp(func(p *SendParams) { p.Phone = "12345" }), phone.ErrInvalid)
	mustErr(t, sp(func(p *SendParams) { p.IdempotencyKey = "" }), ErrInvalidIdempotency)
	mustErr(t, sp(func(p *SendParams) { p.IdempotencyKey = strings.Repeat("k", 256) }), ErrInvalidIdempotency)
	mustErr(t, sp(func(p *SendParams) { p.IdempotencyKey = "bad\x01key" }), ErrInvalidIdempotency)
	mustErr(t, sp(func(p *SendParams) { p.RequestID = uuid.New() }), ErrNotFound)
	other := e.app("OTHR")
	mustErr(t, sp(func(p *SendParams) { p.AppID = other }), ErrNotFound) // app-scoped

	big := e.request(prod, pa.ID, mpesa.MaxAmount+1)
	mustErr(t, sp(func(p *SendParams) { p.RequestID = big.ID }), ErrAmountTooLarge)
	maxed := e.request(prod, pa.ID, mpesa.MaxAmount)
	if err := sp(func(p *SendParams) { p.RequestID = maxed.ID }); err != nil {
		t.Fatalf("max amount: %v", err)
	}

	cancelled := e.request(prod, pa.ID, 100)
	if _, err := e.requests.Cancel(e.ctx, act, prod, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	mustErr(t, sp(func(p *SendParams) { p.RequestID = cancelled.ID }), ErrNotPayable)

	if _, err := e.apps.SetStatus(e.ctx, operator, prod, store.AppStatusSuspended); err != nil {
		t.Fatal(err)
	}
	mustErr(t, sp(func(p *SendParams) {}), ErrNotPayable)

	off := NewService(Options{Pool: e.db.App})
	if off.Available() {
		t.Fatal("service without a client is available")
	}
	if _, _, err := off.Send(e.ctx, act, SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourcePayPage}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("off: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM stk_pushes WHERE payment_request_id <> $1`, maxed.ID); n != 0 {
		t.Fatalf("rejected sends left %d pushes", n)
	}
	e.checkInvariants()
}

func TestSendIdempotencyAndOneLivePush(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 100)
	act := audit.App(prod)
	key := "order-42-push"
	p1, replayed, err := e.svc.Send(e.ctx, act, SendParams{AppID: prod, RequestID: r.ID, Phone: "0712345678", Source: SourceApp, IdempotencyKey: key})
	if err != nil || replayed {
		t.Fatal(err, replayed)
	}
	p2, replayed, err := e.svc.Send(e.ctx, act, SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourceApp, IdempotencyKey: key})
	if err != nil || !replayed || p2.ID != p1.ID {
		t.Fatalf("replay: %v %v %v", p2.ID, replayed, err)
	}
	_, _, err = e.svc.Send(e.ctx, act, SendParams{AppID: prod, RequestID: r.ID, Phone: "0722000111", Source: SourceApp, IdempotencyKey: key})
	mustErr(t, err, ErrIdempotencyConflict)
	r2 := e.request(prod, pa.ID, 100)
	_, _, err = e.svc.Send(e.ctx, act, SendParams{AppID: prod, RequestID: r2.ID, Phone: custPhone, Source: SourceApp, IdempotencyKey: key})
	mustErr(t, err, ErrIdempotencyConflict)

	// Another key while p1 is live.
	_, _, err = e.svc.Send(e.ctx, act, SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourceApp, IdempotencyKey: "other"})
	var ip *InProgressError
	if !errors.As(err, &ip) || ip.LiveID != p1.ID {
		t.Fatalf("live push: %v", err)
	}
	_, _, err = e.svc.Send(e.ctx, act, SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourcePayPage})
	if !errors.As(err, &ip) || ip.LiveID != p1.ID {
		t.Fatalf("live push (pay page): %v", err)
	}
	if len(e.fake.pushes) != 1 {
		t.Fatalf("daraja called %d times", len(e.fake.pushes))
	}
	e.checkInvariants()
}

func TestSendConcurrentSameKey(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 100)
	const n = 8
	type res struct {
		p        Push
		replayed bool
		err      error
	}
	out := make(chan res, n)
	start := make(chan struct{})
	for range n {
		go func() {
			<-start
			p, rp, err := e.svc.Send(e.ctx, audit.App(prod), SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourceApp, IdempotencyKey: "same"})
			out <- res{p, rp, err}
		}()
	}
	close(start)
	var fresh int
	var id uuid.UUID
	for range n {
		x := <-out
		if x.err != nil {
			t.Fatalf("concurrent send: %v", x.err)
		}
		if id == uuid.Nil {
			id = x.p.ID
		}
		if x.p.ID != id {
			t.Fatal("different pushes for one key")
		}
		if !x.replayed {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d fresh pushes", fresh)
	}
	e.checkInvariants()
}

func TestSendDarajaRejectedAndTransient(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 100)
	act := audit.App(prod)

	e.fake.setPushErr(&mpesa.Error{Kind: mpesa.Rejected, HTTPStatus: 400, Code: "400.002.02", Message: "Bad Request - Invalid PhoneNumber"})
	p, _, err := e.svc.Send(e.ctx, act, SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourcePayPage})
	mustErr(t, err, ErrDaraja)
	if p.Status != store.StkPushStatusFailed || *p.ResultDesc != "Bad Request - Invalid PhoneNumber" || *p.ResultCode != "400.002.02" {
		t.Fatalf("rejected push: %+v", p)
	}
	if tx := e.txn(p.TransactionID); tx.Status != store.TxnStatusFailed || *tx.FailureCode != failRejected {
		t.Fatalf("rejected txn: %+v", tx)
	}
	if e.events(p.ID, events.STKPushFailed) != 1 {
		t.Fatal("no failed event")
	}

	// Transient: outcome unknown, push stays initiating, no error.
	e.fake.setPushErr(&mpesa.Error{Kind: mpesa.Transient, Message: "timeout"})
	p2, _, err := e.svc.Send(e.ctx, act, SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourcePayPage})
	if err != nil || p2.Status != store.StkPushStatusInitiating {
		t.Fatalf("transient: %+v %v", p2, err)
	}
	// Sweeper before the timeout: nothing; after: failed not_sent.
	if res, err := e.svc.Sweep(e.ctx, 10); err != nil || res != (SweepResult{}) {
		t.Fatalf("early sweep: %+v %v", res, err)
	}
	e.advance(3 * time.Minute)
	res, err := e.svc.Sweep(e.ctx, 10)
	if err != nil || res.Failed != 1 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if got := e.push(p2.ID); got.Status != store.StkPushStatusFailed {
		t.Fatalf("push: %+v", got)
	}
	if tx := e.txn(p2.TransactionID); tx.Status != store.TxnStatusFailed || *tx.FailureCode != failNotSent {
		t.Fatalf("txn: %+v", tx)
	}
	// A new push is allowed again.
	e.fake.setPushErr(nil)
	e.send(prod, r.ID, custPhone)
	e.checkInvariants()
}

func TestFailureCallbacks(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 100)
	p := e.send(prod, r.ID, custPhone)

	cb := e.deliver(store.MpesaCallbackKindStk, stkFailure(*p.CheckoutRequestID, "1032", "Request cancelled by user"))
	if cb.Error != nil {
		t.Fatalf("callback error: %s", *cb.Error)
	}
	got := e.push(p.ID)
	if got.Status != store.StkPushStatusFailed || *got.ResultCode != "1032" || CustomerMessage(*got.ResultCode) != "You cancelled the M-Pesa prompt." {
		t.Fatalf("push: %+v", got)
	}
	if tx := e.txn(p.TransactionID); tx.Status != store.TxnStatusFailed || *tx.FailureCode != "1032" || *tx.FailureReason != "Request cancelled by user" {
		t.Fatalf("txn: %+v", tx)
	}
	// Duplicate failure: no-op.
	e.deliver(store.MpesaCallbackKindStk, stkFailure(*p.CheckoutRequestID, "1032", "Request cancelled by user"))
	if e.events(p.ID, events.STKPushFailed) != 1 {
		t.Fatal("failed event count")
	}
	// Failure for an unknown checkout id: processed with a note.
	unk := e.deliver(store.MpesaCallbackKindStk, stkFailure("ws_CO_unknown", "1037", "DS timeout"))
	if unk.ProcessedAt == nil || unk.Error == nil {
		t.Fatalf("unknown failure: %+v", unk)
	}

	// Late failure after success: alarm, nothing changes.
	p2 := e.send(prod, r.ID, custPhone)
	e.deliver(store.MpesaCallbackKindStk, stkSuccess(*p2.CheckoutRequestID, newReceipt(), 100, custPhone))
	late := e.deliver(store.MpesaCallbackKindStk, stkFailure(*p2.CheckoutRequestID, "1037", "DS timeout"))
	if late.Error == nil || !strings.Contains(*late.Error, "late failure") {
		t.Fatalf("late failure: %+v", late)
	}
	if e.push(p2.ID).Status != store.StkPushStatusSucceeded || e.txn(p2.TransactionID).Status != store.TxnStatusSucceeded {
		t.Fatal("late failure changed state")
	}
	if !strings.Contains(e.logs.String(), `"alarm":true`) {
		t.Fatal("no alarm logged")
	}
	e.checkInvariants()
}

func TestAmountMismatch(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 500)
	p := e.send(prod, r.ID, custPhone)
	receipt := newReceipt()
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(*p.CheckoutRequestID, receipt, 50, custPhone))
	if cb.Error == nil || !strings.Contains(*cb.Error, "amount mismatch") || cb.ProcessedAt == nil {
		t.Fatalf("callback: %+v", cb)
	}
	tx := e.txn(p.TransactionID)
	if tx.Status != store.TxnStatusNeedsReview || *tx.FailureCode != failMismatch || tx.ProviderTransactionID != nil {
		t.Fatalf("txn: %+v", tx)
	}
	if got := e.push(p.ID); got.Status != store.StkPushStatusUnknown {
		t.Fatalf("push: %+v", got)
	}
	if e.req(prod, r.ID).AmountPaid != 0 || e.events(p.ID, events.STKPushNeedsReview) != 1 {
		t.Fatal("mismatch paid the request or emitted wrong events")
	}
	// Staff cannot confirm a mismatched amount; they can fail it.
	_, err := e.svc.Resolve(e.ctx, operator, ResolveParams{TransactionID: tx.ID, Confirm: true, Receipt: receipt, Reason: "checked portal"})
	mustErr(t, err, ErrNotResolvable)
	got, err := e.svc.Resolve(e.ctx, operator, ResolveParams{TransactionID: tx.ID, Reason: "wrong amount; refund separately"})
	if err != nil || got.Status != store.TxnStatusFailed {
		t.Fatalf("fail: %+v %v", got, err)
	}
	if e.push(p.ID).Status != store.StkPushStatusFailed {
		t.Fatal("push not failed")
	}
	e.checkInvariants()
}

func TestReceiptTaken(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 300)
	receipt := newReceipt()
	// The receipt is first recorded as an unmatched C2B payment.
	if cb := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(receipt, 300, "NOPE-99999", testShortCode)); cb.TransactionID == nil {
		t.Fatal("c2b not recorded")
	}
	p := e.send(prod, r.ID, custPhone)
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(*p.CheckoutRequestID, receipt, 300, custPhone))
	if cb.Error == nil || !strings.Contains(*cb.Error, "already recorded") {
		t.Fatalf("callback: %+v", cb)
	}
	tx := e.txn(p.TransactionID)
	if tx.Status != store.TxnStatusNeedsReview || *tx.FailureCode != failReceiptUsed {
		t.Fatalf("txn: %+v", tx)
	}
	if e.push(p.ID).Status != store.StkPushStatusUnknown || e.req(prod, r.ID).AmountPaid != 0 {
		t.Fatal("receipt taken: wrong state")
	}
	// Staff cannot reuse the receipt either.
	_, err := e.svc.Resolve(e.ctx, operator, ResolveParams{TransactionID: tx.ID, Confirm: true, Receipt: strings.ToLower(receipt), Reason: "portal"})
	mustErr(t, err, ErrReceiptTaken)
	e.checkInvariants()
}

func TestOrphanCallbacks(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)

	// Transient error after Daraja actually showed the prompt: the push has
	// no checkout id; the callback matches it by phone and amount.
	r := e.request(prod, pa.ID, 700)
	e.fake.setPushErr(&mpesa.Error{Kind: mpesa.Transient, Message: "read timeout"})
	p, _, err := e.svc.Send(e.ctx, audit.App(prod), SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourcePayPage})
	if err != nil || p.Status != store.StkPushStatusInitiating {
		t.Fatal(p, err)
	}
	_, co := e.fake.lastPush()
	receipt := newReceipt()
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(co, receipt, 700, custPhone))
	if cb.Error != nil {
		t.Fatalf("orphan: %s", *cb.Error)
	}
	got := e.push(p.ID)
	if got.Status != store.StkPushStatusSucceeded || *got.CheckoutRequestID != co {
		t.Fatalf("orphan push: %+v", got)
	}
	if tx := e.txn(p.TransactionID); tx.Status != store.TxnStatusSucceeded || *tx.ProviderRequestID != co {
		t.Fatalf("orphan txn: %+v", tx)
	}
	if e.req(prod, r.ID).Status != allocation.StatusPaid {
		t.Fatal("orphan: request not paid")
	}

	// Ambiguous: two orphan pushes with the same phone and amount.
	ra := e.request(prod, pa.ID, 250)
	rb := e.request(prod, pa.ID, 250)
	for _, rq := range []uuid.UUID{ra.ID, rb.ID} {
		if _, _, err := e.svc.Send(e.ctx, audit.App(prod), SendParams{AppID: prod, RequestID: rq, Phone: custPhone, Source: SourcePayPage}); err != nil {
			t.Fatal(err)
		}
	}
	amb := e.deliver(store.MpesaCallbackKindStk, stkSuccess("ws_CO_ambiguous", newReceipt(), 250, custPhone))
	if amb.Error == nil || !strings.Contains(*amb.Error, "suspense") || amb.TransactionID == nil {
		t.Fatalf("ambiguous: %+v", amb)
	}
	if tx := e.txn(*amb.TransactionID); tx.Status != store.TxnStatusSucceeded || tx.AppID != nil || tx.PayerAccountID != nil {
		t.Fatalf("unmatched txn: %+v", tx)
	}
	if e.req(prod, ra.ID).AmountPaid != 0 || e.req(prod, rb.ID).AmountPaid != 0 {
		t.Fatal("ambiguous payment allocated")
	}

	// None: no push at all with that phone/amount.
	none := e.deliver(store.MpesaCallbackKindStk, stkSuccess("ws_CO_none", newReceipt(), 999, "254799000111"))
	if none.Error == nil || none.TransactionID == nil || e.txn(*none.TransactionID).AppID != nil {
		t.Fatalf("none: %+v", none)
	}
	e.checkInvariants()
	e.assertNoLeak()
}

func TestDarajaRejectedPushIsNotAnOrphan(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 450)
	e.fake.setPushErr(&mpesa.Error{Kind: mpesa.Rejected, Message: "Invalid Access Token"})
	if _, _, err := e.svc.Send(e.ctx, audit.App(prod), SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourcePayPage}); !errors.Is(err, ErrDaraja) {
		t.Fatal(err)
	}
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess("ws_CO_stranger", newReceipt(), 450, custPhone))
	if cb.Error == nil || !strings.Contains(*cb.Error, "suspense") || e.req(prod, r.ID).AmountPaid != 0 {
		t.Fatalf("rejected push matched: %+v", cb)
	}
	e.checkInvariants()
}

func TestSuccessAfterGiveUp(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 800)
	p := e.send(prod, r.ID, custPhone)
	co := *p.CheckoutRequestID

	// Status query says failed -> push and txn failed.
	e.fake.setQuery(co, mpesa.STKQueryResult{State: mpesa.QueryFailed, ResultCode: "1037", ResultDesc: "DS timeout user cannot be reached"})
	e.advance(2 * time.Minute)
	res, err := e.svc.Sweep(e.ctx, 10)
	if err != nil || res.Queried != 1 || res.Failed != 1 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if e.push(p.ID).Status != store.StkPushStatusFailed || e.txn(p.TransactionID).Status != store.TxnStatusFailed {
		t.Fatal("not failed")
	}
	// ...but the money arrived.
	receipt := newReceipt()
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(co, receipt, 800, custPhone))
	if cb.Error != nil || cb.TransactionID == nil || *cb.TransactionID == p.TransactionID {
		t.Fatalf("callback: %+v", cb)
	}
	nt := e.txn(*cb.TransactionID)
	if nt.Status != store.TxnStatusSucceeded || *nt.ProviderTransactionID != receipt || nt.ProviderRequestID != nil ||
		*nt.PaymentRequestID != r.ID || *nt.PayerAccountID != pa.ID || nt.Channel != store.TxnChannelMpesaStk {
		t.Fatalf("new txn: %+v", nt)
	}
	if got := e.push(p.ID); got.Status != store.StkPushStatusSucceeded {
		t.Fatalf("push: %+v", got)
	}
	if e.req(prod, r.ID).Status != allocation.StatusPaid || e.events(p.ID, events.STKPushSucceeded) != 1 {
		t.Fatal("not paid")
	}
	// Duplicate: no-op.
	dup := e.deliver(store.MpesaCallbackKindStk, stkSuccess(co, receipt, 800, custPhone))
	if dup.Error != nil || *dup.TransactionID != nt.ID {
		t.Fatalf("dup: %+v", dup)
	}
	// A different receipt for the same (already succeeded) push: not recorded.
	other := e.deliver(store.MpesaCallbackKindStk, stkSuccess(co, newReceipt(), 800, custPhone))
	if other.Error == nil {
		t.Fatalf("second receipt recorded: %+v", other)
	}
	if n := e.count(`SELECT count(*) FROM transactions WHERE payment_request_id = $1 AND status = 'succeeded'`, r.ID); n != 1 {
		t.Fatalf("%d succeeded txns", n)
	}
	e.checkInvariants()
}

func TestOrphanAfterNotSent(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 320)
	e.fake.setPushErr(&mpesa.Error{Kind: mpesa.Transient, Message: "EOF"})
	p, _, err := e.svc.Send(e.ctx, audit.App(prod), SendParams{AppID: prod, RequestID: r.ID, Phone: custPhone, Source: SourcePayPage})
	if err != nil {
		t.Fatal(err)
	}
	_, co := e.fake.lastPush()
	e.advance(3 * time.Minute)
	if res, err := e.svc.Sweep(e.ctx, 10); err != nil || res.Failed != 1 {
		t.Fatal(res, err)
	}
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(co, newReceipt(), 320, custPhone))
	if cb.Error != nil {
		t.Fatalf("callback: %s", *cb.Error)
	}
	got := e.push(p.ID)
	if got.Status != store.StkPushStatusSucceeded || *got.CheckoutRequestID != co {
		t.Fatalf("push: %+v", got)
	}
	nt := e.txn(*cb.TransactionID)
	if nt.ID == p.TransactionID || nt.ProviderRequestID == nil || *nt.ProviderRequestID != co {
		t.Fatalf("new txn: %+v", nt)
	}
	if e.req(prod, r.ID).Status != allocation.StatusPaid {
		t.Fatal("not paid")
	}
	e.checkInvariants()
}

func TestSweepOutcomes(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)

	// Query success -> unknown + needs_review; then the late callback
	// completes it normally.
	r1 := e.request(prod, pa.ID, 100)
	p1 := e.send(prod, r1.ID, custPhone)
	e.fake.setQuery(*p1.CheckoutRequestID, mpesa.STKQueryResult{State: mpesa.QuerySucceeded, ResultCode: "0", ResultDesc: "processed successfully"})

	// Query pending -> rescheduled with backoff.
	r2 := e.request(prod, pa.ID, 200)
	p2 := e.send(prod, r2.ID, custPhone)

	// Not due yet.
	if res, err := e.svc.Sweep(e.ctx, 10); err != nil || res != (SweepResult{}) {
		t.Fatalf("early: %+v %v", res, err)
	}
	e.advance(61 * time.Second)
	res, err := e.svc.Sweep(e.ctx, 10)
	if err != nil || res.Queried != 2 || res.NeedsReview != 1 || res.Rescheduled != 1 {
		t.Fatalf("sweep: %+v %v", res, err)
	}
	if got := e.push(p1.ID); got.Status != store.StkPushStatusUnknown {
		t.Fatalf("p1: %+v", got)
	}
	tx1 := e.txn(p1.TransactionID)
	if tx1.Status != store.TxnStatusNeedsReview || *tx1.FailureCode != failNoReceipt || tx1.ProviderTransactionID != nil {
		t.Fatalf("tx1: %+v", tx1)
	}
	if e.req(prod, r1.ID).AmountPaid != 0 || e.events(p1.ID, events.STKPushNeedsReview) != 1 {
		t.Fatal("query success recorded money")
	}
	got2 := e.push(p2.ID)
	if got2.Status != store.StkPushStatusSent || got2.QueryAttempts != 1 || got2.NextQueryAt == nil ||
		got2.NextQueryAt.Sub(e.now()) < 50*time.Second || got2.NextQueryAt.Sub(e.now()) > 70*time.Second {
		t.Fatalf("p2: %+v", got2)
	}
	// Lease: an immediate second sweep does nothing.
	if res, err := e.svc.Sweep(e.ctx, 10); err != nil || res != (SweepResult{}) {
		t.Fatalf("second sweep: %+v %v", res, err)
	}

	// Late callback for the unknown push.
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(*p1.CheckoutRequestID, newReceipt(), 100, custPhone))
	if cb.Error != nil || e.txn(p1.TransactionID).Status != store.TxnStatusSucceeded || e.push(p1.ID).Status != store.StkPushStatusSucceeded ||
		e.req(prod, r1.ID).Status != allocation.StatusPaid {
		t.Fatalf("late callback: %+v", cb)
	}

	// Backoff grows: 60s, 2m, 5m, 15m, 15m.
	for i, want := range []time.Duration{2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 15 * time.Minute} {
		e.advance(e.push(p2.ID).NextQueryAt.Sub(e.now()) + time.Second)
		if e.now().Sub(p2.CreatedAt) >= 30*time.Minute {
			break
		}
		if res, err := e.svc.Sweep(e.ctx, 10); err != nil || res.Rescheduled != 1 {
			t.Fatalf("backoff %d: %+v %v", i, res, err)
		}
		got := e.push(p2.ID)
		if d := got.NextQueryAt.Sub(e.now()); d < want-10*time.Second || d > want+10*time.Second {
			t.Fatalf("backoff %d: next in %v, want %v", i, d, want)
		}
	}
	// Past GiveUpAfter -> needs_review "no_answer".
	e.advance(31 * time.Minute)
	res, err = e.svc.Sweep(e.ctx, 10)
	if err != nil || res.NeedsReview != 1 {
		t.Fatalf("give up: %+v %v", res, err)
	}
	if tx := e.txn(p2.TransactionID); tx.Status != store.TxnStatusNeedsReview || *tx.FailureCode != failNoAnswer {
		t.Fatalf("give up txn: %+v", tx)
	}
	if e.push(p2.ID).Status != store.StkPushStatusUnknown {
		t.Fatal("give up push")
	}
	// A late failure callback for the unknown push fails it.
	e.deliver(store.MpesaCallbackKindStk, stkFailure(*p2.CheckoutRequestID, "1019", "Transaction has expired"))
	if e.push(p2.ID).Status != store.StkPushStatusFailed || e.txn(p2.TransactionID).Status != store.TxnStatusFailed {
		t.Fatal("late failure after unknown")
	}
	e.checkInvariants()
}

func TestSweepLosesRaceToCallback(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 100)
	p := e.send(prod, r.ID, custPhone)
	stale := e.push(p.ID)
	e.fake.setQuery(*p.CheckoutRequestID, mpesa.STKQueryResult{State: mpesa.QueryFailed, ResultCode: "1032", ResultDesc: "cancelled"})
	// The callback wins between the claim and the outcome.
	e.deliver(store.MpesaCallbackKindStk, stkSuccess(*p.CheckoutRequestID, newReceipt(), 100, custPhone))
	res, err := e.svc.sweepOne(e.ctx, stale)
	if err != nil || res.Failed != 0 {
		t.Fatalf("sweepOne: %+v %v", res, err)
	}
	if e.push(p.ID).Status != store.StkPushStatusSucceeded || e.txn(p.TransactionID).Status != store.TxnStatusSucceeded {
		t.Fatal("sweeper undid the callback")
	}
	// Query transport error: rescheduled, never failed.
	r2 := e.request(prod, pa.ID, 100)
	p2 := e.send(prod, r2.ID, custPhone)
	e.fake.mu.Lock()
	e.fake.queryErr = &mpesa.Error{Kind: mpesa.Transient, Message: "503"}
	e.fake.mu.Unlock()
	res, err = e.svc.sweepOne(e.ctx, e.push(p2.ID))
	if err != nil || res.Rescheduled != 1 || e.push(p2.ID).Status != store.StkPushStatusSent {
		t.Fatalf("transient query: %+v %v", res, err)
	}
	e.checkInvariants()
}

func TestResolve(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	staff := audit.Admin(uuid.New())

	needsReview := func(amount int64) (Push, uuid.UUID) {
		r := e.request(prod, pa.ID, amount)
		p := e.send(prod, r.ID, custPhone)
		e.fake.setQuery(*p.CheckoutRequestID, mpesa.STKQueryResult{State: mpesa.QuerySucceeded, ResultCode: "0"})
		if _, err := e.svc.sweepOne(e.ctx, e.push(p.ID)); err != nil {
			t.Fatal(err)
		}
		if e.txn(p.TransactionID).Status != store.TxnStatusNeedsReview {
			t.Fatal("setup: not needs_review")
		}
		return p, r.ID
	}

	p, rid := needsReview(900)
	// Validation.
	_, err := e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p.TransactionID, Confirm: true, Receipt: "SK1", Reason: "x"})
	mustErr(t, err, ErrReasonRequired)
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p.TransactionID, Confirm: true, Receipt: "SK-1 2", Reason: "portal"})
	mustErr(t, err, ErrInvalidReceipt)
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p.TransactionID, Confirm: true, Receipt: "", Reason: "portal"})
	mustErr(t, err, ErrInvalidReceipt)
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: uuid.New(), Confirm: true, Receipt: "SK1", Reason: "portal"})
	mustErr(t, err, ErrNotFound)

	// Confirm: receipt normalised, allocated, push succeeded, audited.
	receipt := newReceipt()
	got, err := e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p.TransactionID, Confirm: true, Receipt: " " + strings.ToLower(receipt) + " ", Reason: "seen on M-Pesa portal"})
	if err != nil || got.Status != store.TxnStatusSucceeded || *got.ProviderTransactionID != receipt {
		t.Fatalf("confirm: %+v %v", got, err)
	}
	if e.req(prod, rid).Status != allocation.StatusPaid || e.push(p.ID).Status != store.StkPushStatusSucceeded ||
		e.events(p.ID, events.STKPushSucceeded) != 1 {
		t.Fatal("confirm did not complete")
	}
	if e.count(`SELECT count(*) FROM audit_log WHERE action = 'transaction.resolved' AND entity_id = $1 AND details->>'receipt' = $2`, p.TransactionID.String(), receipt) != 1 {
		t.Fatal("no audit entry")
	}
	// Not resolvable any more (succeeded); the late callback is a no-op.
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p.TransactionID, Reason: "again"})
	mustErr(t, err, ErrNotResolvable)
	if cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(*p.CheckoutRequestID, receipt, 900, custPhone)); cb.Error != nil {
		t.Fatalf("late callback after confirm: %s", *cb.Error)
	}

	// Receipt already used elsewhere.
	p2, _ := needsReview(400)
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p2.TransactionID, Confirm: true, Receipt: receipt, Reason: "portal"})
	mustErr(t, err, ErrReceiptTaken)

	// Fail.
	got, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p2.TransactionID, Reason: "not on the statement"})
	if err != nil || got.Status != store.TxnStatusFailed {
		t.Fatalf("fail: %+v %v", got, err)
	}
	if e.push(p2.ID).Status != store.StkPushStatusFailed || e.events(p2.ID, events.STKPushFailed) != 1 {
		t.Fatal("fail: push")
	}

	// Pending (not in review) and C2B succeeded: not resolvable.
	r3 := e.request(prod, pa.ID, 100)
	p3 := e.send(prod, r3.ID, custPhone)
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p3.TransactionID, Reason: "too early"})
	mustErr(t, err, ErrNotResolvable)
	c := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 10, pa.AccountNumber, testShortCode))
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: *c.TransactionID, Reason: "nope"})
	mustErr(t, err, ErrNotResolvable)
	e.checkInvariants()
}

func TestC2B(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 1000)

	receipt := newReceipt()
	cb := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(receipt, 1000, strings.ToLower(pa.AccountNumber), testShortCode))
	if cb.Error != nil || cb.TransactionID == nil {
		t.Fatalf("c2b: %+v", cb)
	}
	tx := e.txn(*cb.TransactionID)
	if tx.Channel != store.TxnChannelMpesaC2b || *tx.PayerAccountID != pa.ID || tx.Status != store.TxnStatusSucceeded {
		t.Fatalf("txn: %+v", tx)
	}
	if e.req(prod, r.ID).Status != allocation.StatusPaid {
		t.Fatal("not paid")
	}
	// Duplicate receipt: no-op.
	dup := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(receipt, 1000, pa.AccountNumber, testShortCode))
	if dup.TransactionID == nil || *dup.TransactionID != tx.ID {
		t.Fatalf("dup: %+v", dup)
	}
	// Same receipt, different amount: alarm, nothing recorded.
	conflict := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(receipt, 5, pa.AccountNumber, testShortCode))
	if conflict.Error == nil || !strings.Contains(*conflict.Error, "different details") {
		t.Fatalf("conflict: %+v", conflict)
	}
	// Unknown account: suspense.
	unk := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 70, "WHO-1", testShortCode))
	if unk.TransactionID == nil || e.txn(*unk.TransactionID).AppID != nil || e.txn(*unk.TransactionID).Status != store.TxnStatusSucceeded {
		t.Fatalf("unknown: %+v", unk)
	}
	// Another short code: not recorded.
	before := e.count(`SELECT count(*) FROM transactions`)
	foreign := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 70, pa.AccountNumber, "600000"))
	if foreign.Error == nil || foreign.TransactionID != nil || e.count(`SELECT count(*) FROM transactions`) != before {
		t.Fatalf("foreign short code: %+v", foreign)
	}
	// Malformed.
	bad := e.deliver(store.MpesaCallbackKindC2bConfirmation, `{"TransID":"ABC","TransAmount":"10.50"}`)
	if bad.Error == nil || bad.ProcessedAt == nil {
		t.Fatalf("malformed: %+v", bad)
	}
	badSTK := e.deliver(store.MpesaCallbackKindStk, `{"hello":"world"}`)
	if badSTK.Error == nil || badSTK.ProcessedAt == nil {
		t.Fatalf("malformed stk: %+v", badSTK)
	}

	// Suspended app: still allocated, with a warning.
	if _, err := e.apps.SetStatus(e.ctx, operator, prod, store.AppStatusSuspended); err != nil {
		t.Fatal(err)
	}
	in := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 60, pa.AccountNumber, testShortCode))
	if in.Error != nil || in.TransactionID == nil || e.txn(*in.TransactionID).PayerAccountID == nil {
		t.Fatalf("inactive: %+v", in)
	}
	if !strings.Contains(e.logs.String(), "inactive app or closed payer") {
		t.Fatal("no inactive warning")
	}
	e.checkInvariants()
	e.assertNoLeak()
}

func TestSuccessCallbackWithoutReceipt(t *testing.T) {
	e := newEnv(t)
	prod := e.app("HOST")
	pa := e.payer(prod)
	r := e.request(prod, pa.ID, 100)
	p := e.send(prod, r.ID, custPhone)
	raw := `{"Body":{"stkCallback":{"MerchantRequestID":"m","CheckoutRequestID":"` + *p.CheckoutRequestID +
		`","ResultCode":0,"ResultDesc":"ok","CallbackMetadata":{"Item":[{"Name":"Amount","Value":100}]}}}}`
	cb := e.deliver(store.MpesaCallbackKindStk, raw)
	if cb.Error == nil {
		t.Fatal("no error recorded")
	}
	if e.txn(p.TransactionID).Status != store.TxnStatusNeedsReview || e.push(p.ID).Status != store.StkPushStatusUnknown {
		t.Fatal("not sent to review")
	}
	e.checkInvariants()
}
