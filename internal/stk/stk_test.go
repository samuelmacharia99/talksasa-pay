package stk

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/store"
)

const custPhone = "254712345678"

func TestSendAndSuccessCallback(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 1500)

	p := e.send(app, c.ID, "0712 345 678")
	if p.Status != store.StkPushStatusSent || p.CheckoutRequestID == nil || p.Amount != 1500 || p.Phone != custPhone {
		t.Fatalf("push after send: %+v", p)
	}
	req, co := e.fake.lastPush()
	if req.AccountReference != c.AccountNumber || req.TransactionDesc != c.Reference || req.Amount != 1500 || req.Phone != custPhone {
		t.Fatalf("daraja request: %+v", req)
	}
	if *p.CheckoutRequestID != co {
		t.Fatalf("checkout id %s, want %s", *p.CheckoutRequestID, co)
	}
	pending := e.txn(p.TransactionID)
	if pending.Status != store.TxnStatusPending || pending.ProviderRequestID == nil || *pending.ProviderRequestID != co ||
		*pending.AccountNumber != c.AccountNumber || pending.Channel != store.TxnChannelMpesaStk || *pending.ChargeID != c.ID {
		t.Fatalf("pending txn: %+v", pending)
	}
	if e.events(p.ID, events.STKPushSent) != 1 {
		t.Fatal("stk_push.sent not emitted")
	}

	// Get / LatestForCharge / ListForCharge.
	if got, err := e.svc.Get(e.ctx, app, p.ID); err != nil || got.ID != p.ID {
		t.Fatalf("Get: %v", err)
	}
	if _, err := e.svc.Get(e.ctx, e.app("OTHR"), p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get other app: %v", err)
	}
	if l, err := e.svc.LatestForCharge(e.ctx, c.ID, time.Now().Add(-time.Minute)); err != nil || l == nil || l.ID != p.ID {
		t.Fatalf("LatestForCharge: %v %v", l, err)
	}
	if l, err := e.svc.LatestForCharge(e.ctx, c.ID, time.Now().Add(time.Minute)); err != nil || l != nil {
		t.Fatalf("LatestForCharge future: %v %v", l, err)
	}
	if ls, err := e.svc.ListForCharge(e.ctx, c.ID, 10); err != nil || len(ls) != 1 {
		t.Fatalf("ListForCharge: %v %v", ls, err)
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
	if got := e.chargeState(app, c.ID); got.Status != store.ChargeStatusSucceeded || *got.TransactionID != p.TransactionID || got.SucceededAt == nil {
		t.Fatalf("charge: %+v", got)
	}
	if got := e.push(p.ID); got.Status != store.StkPushStatusSucceeded || got.CompletedAt == nil || *got.ResultCode != "0" {
		t.Fatalf("push: %+v", got)
	}
	if e.events(p.ID, events.STKPushSucceeded) != 1 || e.events(c.ID, events.ChargeSucceeded) != 1 {
		t.Fatal("events missing")
	}
	// charge.succeeded carries the succeeded prompt and the paying transaction.
	if n := e.count(`SELECT count(*) FROM events WHERE subject_id = $1 AND type = 'charge.succeeded'
		AND data->>'status' = 'succeeded' AND data->>'transaction_id' = $2 AND data->'latest_stk_push'->>'status' = 'succeeded'`,
		c.ID, p.TransactionID.String()); n != 1 {
		t.Fatal("charge.succeeded payload")
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
		e.events(p.ID, events.STKPushSucceeded) != 1 || e.events(c.ID, events.ChargeSucceeded) != 1 {
		t.Fatal("duplicate changed something")
	}
	// Paid: no more pushes.
	_, _, err := e.svc.Send(e.ctx, audit.App(app), SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourcePayPage})
	mustErr(t, err, ErrNotPayable)
	e.checkInvariants()
	e.assertNoLeak()
}

// A paybill payment for a different amount does not pay the charge
// (payment.received); the prompt still asks for the charge's exact amount.
func TestSendAsksForChargeAmount(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 1000)
	cb := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 400, c.AccountNumber, testShortCode))
	if cb.Error != nil || cb.TransactionID == nil {
		t.Fatalf("c2b: %+v", cb)
	}
	if e.chargeState(app, c.ID).Status != store.ChargeStatusPending || e.events(*cb.TransactionID, events.PaymentReceived) != 1 {
		t.Fatal("partial paybill payment paid the charge")
	}
	p, _, err := e.svc.Send(e.ctx, audit.App(app), SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourcePayPage})
	if err != nil {
		t.Fatal(err)
	}
	if p.Amount != 1000 || p.Source != string(SourcePayPage) || p.IdempotencyKey != nil {
		t.Fatalf("push: %+v", p)
	}
	e.checkInvariants()
}

func TestSendValidation(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 100)
	act := audit.App(app)
	sp := func(mut func(*SendParams)) error {
		p := SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourceApp, IdempotencyKey: uuid.NewString()}
		mut(&p)
		_, _, err := e.svc.Send(e.ctx, act, p)
		return err
	}
	mustErr(t, sp(func(p *SendParams) { p.Phone = "12345" }), phone.ErrInvalid)
	mustErr(t, sp(func(p *SendParams) { p.IdempotencyKey = "" }), ErrInvalidIdempotency)
	mustErr(t, sp(func(p *SendParams) { p.IdempotencyKey = strings.Repeat("k", 256) }), ErrInvalidIdempotency)
	mustErr(t, sp(func(p *SendParams) { p.IdempotencyKey = "bad\x01key" }), ErrInvalidIdempotency)
	mustErr(t, sp(func(p *SendParams) { p.ChargeID = uuid.New() }), ErrNotFound)
	other := e.app("OTHR")
	mustErr(t, sp(func(p *SendParams) { p.AppID = other }), ErrNotFound) // app-scoped

	// A charge can never exceed the M-Pesa limit (both are KES 250,000), so
	// ErrAmountTooLarge is a guard only; the largest charge is promptable.
	maxed := e.charge(app, mpesa.MaxAmount)
	if err := sp(func(p *SendParams) { p.ChargeID = maxed.ID }); err != nil {
		t.Fatalf("max amount: %v", err)
	}

	cancelled := e.charge(app, 100)
	if _, err := store.New(e.db.App).CancelCharge(e.ctx, store.CancelChargeParams{AppID: app, ID: cancelled.ID}); err != nil {
		t.Fatal(err)
	}
	mustErr(t, sp(func(p *SendParams) { p.ChargeID = cancelled.ID }), ErrNotPayable)
	// Past its expiry but not yet swept: not payable by prompt.
	e.advance(25 * time.Hour)
	mustErr(t, sp(func(p *SendParams) {}), ErrNotPayable)
	e.offset.Store(0)

	if _, err := e.apps.SetStatus(e.ctx, operator, app, store.AppStatusSuspended); err != nil {
		t.Fatal(err)
	}
	mustErr(t, sp(func(p *SendParams) {}), ErrNotPayable)

	off := NewService(Options{Pool: e.db.App})
	if off.Available() {
		t.Fatal("service without a client is available")
	}
	if _, _, err := off.Send(e.ctx, act, SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourcePayPage}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("off: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM stk_pushes WHERE charge_id <> $1`, maxed.ID); n != 0 {
		t.Fatalf("rejected sends left %d pushes", n)
	}
	e.checkInvariants()
}

func TestSendIdempotencyAndOneLivePush(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 100)
	act := audit.App(app)
	key := "order-42-push"
	p1, replayed, err := e.svc.Send(e.ctx, act, SendParams{AppID: app, ChargeID: c.ID, Phone: "0712345678", Source: SourceApp, IdempotencyKey: key})
	if err != nil || replayed {
		t.Fatal(err, replayed)
	}
	p2, replayed, err := e.svc.Send(e.ctx, act, SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourceApp, IdempotencyKey: key})
	if err != nil || !replayed || p2.ID != p1.ID {
		t.Fatalf("replay: %v %v %v", p2.ID, replayed, err)
	}
	_, _, err = e.svc.Send(e.ctx, act, SendParams{AppID: app, ChargeID: c.ID, Phone: "0722000111", Source: SourceApp, IdempotencyKey: key})
	mustErr(t, err, ErrIdempotencyConflict)
	c2 := e.charge(app, 100)
	_, _, err = e.svc.Send(e.ctx, act, SendParams{AppID: app, ChargeID: c2.ID, Phone: custPhone, Source: SourceApp, IdempotencyKey: key})
	mustErr(t, err, ErrIdempotencyConflict)

	// Another key while p1 is live.
	_, _, err = e.svc.Send(e.ctx, act, SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourceApp, IdempotencyKey: "other"})
	var ip *InProgressError
	if !errors.As(err, &ip) || ip.LiveID != p1.ID {
		t.Fatalf("live push: %v", err)
	}
	_, _, err = e.svc.Send(e.ctx, act, SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourcePayPage})
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
	app := e.app("HOST")
	c := e.charge(app, 100)
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
			p, rp, err := e.svc.Send(e.ctx, audit.App(app), SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourceApp, IdempotencyKey: "same"})
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

// Rule 4: a failed prompt does not fail the charge; it can be prompted again.
func TestSendDarajaRejectedAndTransient(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 100)
	act := audit.App(app)

	e.fake.setPushErr(&mpesa.Error{Kind: mpesa.Rejected, HTTPStatus: 400, Code: "400.002.02", Message: "Bad Request - Invalid PhoneNumber"})
	p, _, err := e.svc.Send(e.ctx, act, SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourcePayPage})
	mustErr(t, err, ErrDaraja)
	if p.Status != store.StkPushStatusFailed || *p.ResultDesc != "Bad Request - Invalid PhoneNumber" || *p.ResultCode != "400.002.02" {
		t.Fatalf("rejected push: %+v", p)
	}
	if tx := e.txn(p.TransactionID); tx.Status != store.TxnStatusFailed || *tx.FailureCode != failRejected {
		t.Fatalf("rejected txn: %+v", tx)
	}
	if e.events(p.ID, events.STKPushFailed) != 1 || e.chargeState(app, c.ID).Status != store.ChargeStatusPending {
		t.Fatal("no failed event, or the charge changed")
	}

	// Transient: outcome unknown, push stays initiating, no error.
	e.fake.setPushErr(&mpesa.Error{Kind: mpesa.Transient, Message: "timeout"})
	p2, _, err := e.svc.Send(e.ctx, act, SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourcePayPage})
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
	// A new push is allowed again; the charge stayed pending throughout.
	e.fake.setPushErr(nil)
	e.send(app, c.ID, custPhone)
	if e.chargeState(app, c.ID).Status != store.ChargeStatusPending {
		t.Fatal("charge not pending")
	}
	e.checkInvariants()
}

func TestFailureCallbacks(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 100)
	p := e.send(app, c.ID, custPhone)

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
	if e.chargeState(app, c.ID).Status != store.ChargeStatusPending {
		t.Fatal("failed prompt failed the charge")
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
	p2 := e.send(app, c.ID, custPhone)
	e.deliver(store.MpesaCallbackKindStk, stkSuccess(*p2.CheckoutRequestID, newReceipt(), 100, custPhone))
	late := e.deliver(store.MpesaCallbackKindStk, stkFailure(*p2.CheckoutRequestID, "1037", "DS timeout"))
	if late.Error == nil || !strings.Contains(*late.Error, "late failure") {
		t.Fatalf("late failure: %+v", late)
	}
	if e.push(p2.ID).Status != store.StkPushStatusSucceeded || e.txn(p2.TransactionID).Status != store.TxnStatusSucceeded ||
		e.chargeState(app, c.ID).Status != store.ChargeStatusSucceeded {
		t.Fatal("late failure changed state")
	}
	if !strings.Contains(e.logs.String(), `"alarm":true`) {
		t.Fatal("no alarm logged")
	}
	e.checkInvariants()
}

// Rule 3: an STK success for a different amount records nothing, raises an
// alarm and blocks staff from confirming it.
func TestAmountMismatch(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 500)
	p := e.send(app, c.ID, custPhone)
	receipt := newReceipt()
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(*p.CheckoutRequestID, receipt, 50, custPhone))
	if cb.Error == nil || !strings.Contains(*cb.Error, "amount mismatch") || cb.ProcessedAt == nil {
		t.Fatalf("callback: %+v", cb)
	}
	tx := e.txn(p.TransactionID)
	if tx.Status != store.TxnStatusNeedsReview || *tx.FailureCode != failMismatch || tx.ProviderTransactionID != nil {
		t.Fatalf("txn: %+v", tx)
	}
	if got := e.push(p.ID); got.Status != store.StkPushStatusUnknown || got.ConfirmBlocked == nil || *got.ConfirmBlocked != failMismatch {
		t.Fatalf("push: %+v", got)
	}
	if e.count(`SELECT count(*) FROM transactions WHERE provider_transaction_id = $1`, receipt) != 0 {
		t.Fatal("mismatched receipt recorded")
	}
	if got := e.chargeState(app, c.ID); got.Status != store.ChargeStatusNeedsReview || e.events(p.ID, events.STKPushNeedsReview) != 1 ||
		e.events(c.ID, events.ChargeNeedsReview) != 1 || e.events(c.ID, events.ChargeSucceeded) != 0 {
		t.Fatalf("mismatch paid the charge or emitted wrong events: %+v", got)
	}
	// Staff cannot confirm a mismatched amount; they can fail it.
	_, err := e.svc.Resolve(e.ctx, operator, ResolveParams{TransactionID: tx.ID, Confirm: true, Receipt: receipt, Reason: "checked portal"})
	mustErr(t, err, ErrNotResolvable)
	got, err := e.svc.Resolve(e.ctx, operator, ResolveParams{TransactionID: tx.ID, Reason: "wrong amount; refund separately"})
	if err != nil || got.Status != store.TxnStatusFailed {
		t.Fatalf("fail: %+v %v", got, err)
	}
	if e.push(p.ID).Status != store.StkPushStatusFailed || e.chargeState(app, c.ID).Status != store.ChargeStatusPending {
		t.Fatal("push not failed or charge not back to pending")
	}
	e.checkInvariants()
}

func TestReceiptTaken(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 300)
	receipt := newReceipt()
	// The receipt is first recorded as an unmatched C2B payment.
	if cb := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(receipt, 300, "NOPE-99999", testShortCode)); cb.TransactionID == nil {
		t.Fatal("c2b not recorded")
	}
	p := e.send(app, c.ID, custPhone)
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(*p.CheckoutRequestID, receipt, 300, custPhone))
	if cb.Error == nil || !strings.Contains(*cb.Error, "already recorded") {
		t.Fatalf("callback: %+v", cb)
	}
	tx := e.txn(p.TransactionID)
	if tx.Status != store.TxnStatusNeedsReview || *tx.FailureCode != failReceiptUsed {
		t.Fatalf("txn: %+v", tx)
	}
	if e.push(p.ID).Status != store.StkPushStatusUnknown || e.chargeState(app, c.ID).Status != store.ChargeStatusNeedsReview {
		t.Fatal("receipt taken: wrong state")
	}
	// Staff cannot reuse the receipt either.
	_, err := e.svc.Resolve(e.ctx, operator, ResolveParams{TransactionID: tx.ID, Confirm: true, Receipt: strings.ToLower(receipt), Reason: "portal"})
	mustErr(t, err, ErrReceiptTaken)
	e.checkInvariants()
}

func TestOrphanCallbacks(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")

	// Transient error after Daraja actually showed the prompt: the push has
	// no checkout id; the callback matches it by phone and amount.
	c := e.charge(app, 700)
	e.fake.setPushErr(&mpesa.Error{Kind: mpesa.Transient, Message: "read timeout"})
	p, _, err := e.svc.Send(e.ctx, audit.App(app), SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourcePayPage})
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
	if e.chargeState(app, c.ID).Status != store.ChargeStatusSucceeded {
		t.Fatal("orphan: charge not paid")
	}

	// Ambiguous: two orphan pushes with the same phone and amount.
	ca := e.charge(app, 250)
	cbb := e.charge(app, 250)
	for _, id := range []uuid.UUID{ca.ID, cbb.ID} {
		if _, _, err := e.svc.Send(e.ctx, audit.App(app), SendParams{AppID: app, ChargeID: id, Phone: custPhone, Source: SourcePayPage}); err != nil {
			t.Fatal(err)
		}
	}
	amb := e.deliver(store.MpesaCallbackKindStk, stkSuccess("ws_CO_ambiguous", newReceipt(), 250, custPhone))
	if amb.Error == nil || !strings.Contains(*amb.Error, "unmatched") || amb.TransactionID == nil {
		t.Fatalf("ambiguous: %+v", amb)
	}
	if tx := e.txn(*amb.TransactionID); tx.Status != store.TxnStatusSucceeded || tx.AppID != nil || tx.ChargeID != nil {
		t.Fatalf("unmatched txn: %+v", tx)
	}
	if e.chargeState(app, ca.ID).Status != store.ChargeStatusPending || e.chargeState(app, cbb.ID).Status != store.ChargeStatusPending {
		t.Fatal("ambiguous payment paid a charge")
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
	app := e.app("HOST")
	c := e.charge(app, 450)
	e.fake.setPushErr(&mpesa.Error{Kind: mpesa.Rejected, Message: "Invalid Access Token"})
	if _, _, err := e.svc.Send(e.ctx, audit.App(app), SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourcePayPage}); !errors.Is(err, ErrDaraja) {
		t.Fatal(err)
	}
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess("ws_CO_stranger", newReceipt(), 450, custPhone))
	if cb.Error == nil || !strings.Contains(*cb.Error, "unmatched") || e.chargeState(app, c.ID).Status != store.ChargeStatusPending {
		t.Fatalf("rejected push matched: %+v", cb)
	}
	e.checkInvariants()
}

// Rule 6: late money after a given-up prompt is recorded on a new
// transaction, pays the charge, and the push points at it.
func TestSuccessAfterGiveUp(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 800)
	p := e.send(app, c.ID, custPhone)
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
	// The app cancels the charge meanwhile...
	if _, err := store.New(e.db.App).CancelCharge(e.ctx, store.CancelChargeParams{AppID: app, ID: c.ID}); err != nil {
		t.Fatal(err)
	}
	// ...but the money arrived.
	receipt := newReceipt()
	cb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(co, receipt, 800, custPhone))
	if cb.Error != nil || cb.TransactionID == nil || *cb.TransactionID == p.TransactionID {
		t.Fatalf("callback: %+v", cb)
	}
	nt := e.txn(*cb.TransactionID)
	if nt.Status != store.TxnStatusSucceeded || *nt.ProviderTransactionID != receipt || nt.ProviderRequestID != nil ||
		*nt.ChargeID != c.ID || *nt.AppID != app || nt.Channel != store.TxnChannelMpesaStk {
		t.Fatalf("new txn: %+v", nt)
	}
	if got := e.push(p.ID); got.Status != store.StkPushStatusSucceeded || got.PaidTransactionID == nil || *got.PaidTransactionID != nt.ID {
		t.Fatalf("push: %+v", got)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE subject_id = $1 AND type = 'stk_push.succeeded' AND data->>'transaction_id' = $2`,
		p.ID, nt.ID.String()); n != 1 {
		t.Fatal("stk_push.succeeded does not name the new transaction")
	}
	if got := e.chargeState(app, c.ID); got.Status != store.ChargeStatusSucceeded || *got.TransactionID != nt.ID || e.events(c.ID, events.ChargeSucceeded) != 1 {
		t.Fatalf("cancelled charge not paid by late money: %+v", got)
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
	if n := e.count(`SELECT count(*) FROM transactions WHERE charge_id = $1 AND status = 'succeeded'`, c.ID); n != 1 {
		t.Fatalf("%d succeeded txns", n)
	}
	e.checkInvariants()
}

func TestOrphanAfterNotSent(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 320)
	e.fake.setPushErr(&mpesa.Error{Kind: mpesa.Transient, Message: "EOF"})
	p, _, err := e.svc.Send(e.ctx, audit.App(app), SendParams{AppID: app, ChargeID: c.ID, Phone: custPhone, Source: SourcePayPage})
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
	if e.chargeState(app, c.ID).Status != store.ChargeStatusSucceeded {
		t.Fatal("not paid")
	}
	e.checkInvariants()
}

// Rule 5: a status check that says paid never records money; the
// transaction and the charge wait in needs_review.
func TestSweepOutcomes(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")

	// Query success -> unknown + needs_review; then the late callback
	// completes it normally.
	c1 := e.charge(app, 100)
	p1 := e.send(app, c1.ID, custPhone)
	e.fake.setQuery(*p1.CheckoutRequestID, mpesa.STKQueryResult{State: mpesa.QuerySucceeded, ResultCode: "0", ResultDesc: "processed successfully"})

	// Query pending -> rescheduled with backoff.
	c2 := e.charge(app, 200)
	p2 := e.send(app, c2.ID, custPhone)

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
	if e.chargeState(app, c1.ID).Status != store.ChargeStatusNeedsReview || e.events(p1.ID, events.STKPushNeedsReview) != 1 ||
		e.events(c1.ID, events.ChargeNeedsReview) != 1 {
		t.Fatal("query success: charge not in review")
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
		e.chargeState(app, c1.ID).Status != store.ChargeStatusSucceeded {
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
	if e.push(p2.ID).Status != store.StkPushStatusUnknown || e.chargeState(app, c2.ID).Status != store.ChargeStatusNeedsReview {
		t.Fatal("give up push")
	}
	// A late failure callback for the unknown push fails it; the charge goes
	// back to pending.
	e.deliver(store.MpesaCallbackKindStk, stkFailure(*p2.CheckoutRequestID, "1019", "Transaction has expired"))
	if e.push(p2.ID).Status != store.StkPushStatusFailed || e.txn(p2.TransactionID).Status != store.TxnStatusFailed ||
		e.chargeState(app, c2.ID).Status != store.ChargeStatusPending {
		t.Fatal("late failure after unknown")
	}
	e.checkInvariants()
}

func TestSweepLosesRaceToCallback(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 100)
	p := e.send(app, c.ID, custPhone)
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
	c2 := e.charge(app, 100)
	p2 := e.send(app, c2.ID, custPhone)
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
	app := e.app("HOST")
	staff := audit.Admin(uuid.New())

	needsReview := func(amount int64) (Push, uuid.UUID) {
		c := e.charge(app, amount)
		p := e.send(app, c.ID, custPhone)
		e.fake.setQuery(*p.CheckoutRequestID, mpesa.STKQueryResult{State: mpesa.QuerySucceeded, ResultCode: "0"})
		if _, err := e.svc.sweepOne(e.ctx, e.push(p.ID)); err != nil {
			t.Fatal(err)
		}
		if e.txn(p.TransactionID).Status != store.TxnStatusNeedsReview || e.chargeState(app, c.ID).Status != store.ChargeStatusNeedsReview {
			t.Fatal("setup: not needs_review")
		}
		return p, c.ID
	}

	p, cid := needsReview(900)
	// Validation.
	_, err := e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p.TransactionID, Confirm: true, Receipt: "SK1", Reason: "x"})
	mustErr(t, err, ErrReasonRequired)
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p.TransactionID, Confirm: true, Receipt: "SK-1 2", Reason: "portal"})
	mustErr(t, err, ErrInvalidReceipt)
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p.TransactionID, Confirm: true, Receipt: "", Reason: "portal"})
	mustErr(t, err, ErrInvalidReceipt)
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: uuid.New(), Confirm: true, Receipt: "SK1", Reason: "portal"})
	mustErr(t, err, ErrNotFound)

	// Confirm: receipt normalised, charge paid, push succeeded, audited.
	receipt := newReceipt()
	got, err := e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p.TransactionID, Confirm: true, Receipt: " " + strings.ToLower(receipt) + " ", Reason: "seen on M-Pesa portal"})
	if err != nil || got.Status != store.TxnStatusSucceeded || *got.ProviderTransactionID != receipt {
		t.Fatalf("confirm: %+v %v", got, err)
	}
	if e.chargeState(app, cid).Status != store.ChargeStatusSucceeded || e.push(p.ID).Status != store.StkPushStatusSucceeded ||
		e.events(p.ID, events.STKPushSucceeded) != 1 || e.events(cid, events.ChargeSucceeded) != 1 {
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
	p2, cid2 := needsReview(400)
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p2.TransactionID, Confirm: true, Receipt: receipt, Reason: "portal"})
	mustErr(t, err, ErrReceiptTaken)

	// Fail: no money; the charge goes back to pending and can be prompted.
	got, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p2.TransactionID, Reason: "not on the statement"})
	if err != nil || got.Status != store.TxnStatusFailed {
		t.Fatalf("fail: %+v %v", got, err)
	}
	if e.push(p2.ID).Status != store.StkPushStatusFailed || e.events(p2.ID, events.STKPushFailed) != 1 ||
		e.chargeState(app, cid2).Status != store.ChargeStatusPending {
		t.Fatal("fail: push or charge")
	}
	e.send(app, cid2, custPhone)

	// Pending (not in review) and C2B succeeded: not resolvable.
	c3 := e.charge(app, 100)
	p3 := e.send(app, c3.ID, custPhone)
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: p3.TransactionID, Reason: "too early"})
	mustErr(t, err, ErrNotResolvable)
	c := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 10, c3.AccountNumber, testShortCode))
	_, err = e.svc.Resolve(e.ctx, staff, ResolveParams{TransactionID: *c.TransactionID, Reason: "nope"})
	mustErr(t, err, ErrNotResolvable)
	e.checkInvariants()
}

// Staff fail a review after the charge expired: it goes back to pending and
// the expiry job (not the review) expires it later. A needs_review charge
// is never expired.
func TestResolveFailPastExpiry(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 250)
	p := e.send(app, c.ID, custPhone)
	e.fake.setQuery(*p.CheckoutRequestID, mpesa.STKQueryResult{State: mpesa.QuerySucceeded, ResultCode: "0"})
	if _, err := e.svc.sweepOne(e.ctx, e.push(p.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Owner.Exec(e.ctx, `ALTER TABLE charges DISABLE TRIGGER charges_guard`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Owner.Exec(e.ctx, `UPDATE charges SET expires_at = now() - interval '1 hour' WHERE id = $1`, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Owner.Exec(e.ctx, `ALTER TABLE charges ENABLE TRIGGER charges_guard`); err != nil {
		t.Fatal(err)
	}
	if n, err := store.New(e.db.App).ExpireDueCharges(e.ctx, 10); err != nil || len(n) != 0 {
		t.Fatalf("needs_review charge expired: %v %v", n, err)
	}
	if _, err := e.svc.Resolve(e.ctx, operator, ResolveParams{TransactionID: p.TransactionID, Reason: "not on the statement"}); err != nil {
		t.Fatal(err)
	}
	if e.chargeState(app, c.ID).Status != store.ChargeStatusPending {
		t.Fatal("charge not back to pending")
	}
	if n, err := store.New(e.db.App).ExpireDueCharges(e.ctx, 10); err != nil || len(n) != 1 {
		t.Fatalf("expire after review: %v %v", n, err)
	}
	e.checkInvariants()
}

// Rule 7: paybill payments by account-number prefix.
func TestC2B(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 1000)

	receipt := newReceipt()
	cb := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(receipt, 1000, strings.ToLower(c.AccountNumber), testShortCode))
	if cb.Error != nil || cb.TransactionID == nil {
		t.Fatalf("c2b: %+v", cb)
	}
	tx := e.txn(*cb.TransactionID)
	if tx.Channel != store.TxnChannelMpesaC2b || *tx.AppID != app || *tx.ChargeID != c.ID || tx.Status != store.TxnStatusSucceeded {
		t.Fatalf("txn: %+v", tx)
	}
	if got := e.chargeState(app, c.ID); got.Status != store.ChargeStatusSucceeded || *got.TransactionID != tx.ID || e.events(c.ID, events.ChargeSucceeded) != 1 {
		t.Fatal("not paid")
	}
	// Duplicate receipt: no-op.
	dup := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(receipt, 1000, c.AccountNumber, testShortCode))
	if dup.TransactionID == nil || *dup.TransactionID != tx.ID {
		t.Fatalf("dup: %+v", dup)
	}
	// Same receipt, different amount: alarm, nothing recorded.
	conflict := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(receipt, 5, c.AccountNumber, testShortCode))
	if conflict.Error == nil || !strings.Contains(*conflict.Error, "different details") {
		t.Fatalf("conflict: %+v", conflict)
	}
	// Paid twice: its own transaction, payment.received duplicate_of_charge.
	twice := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 1000, c.AccountNumber, testShortCode))
	if twice.TransactionID == nil || e.txn(*twice.TransactionID).ChargeID != nil {
		t.Fatalf("second payment: %+v", twice) // the charge is no longer live: no match, forwarded
	}
	if e.events(*twice.TransactionID, events.PaymentReceived) != 1 {
		t.Fatal("second paybill payment not forwarded")
	}
	// Prefix known, no matching charge: forwarded as payment.received.
	fwd := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 77, "HOST-9999", testShortCode))
	if fwd.TransactionID == nil || *e.txn(*fwd.TransactionID).AppID != app || e.txn(*fwd.TransactionID).ChargeID != nil ||
		e.events(*fwd.TransactionID, events.PaymentReceived) != 1 {
		t.Fatalf("forward: %+v", fwd)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE subject_id = $1 AND data->>'account_number' = 'HOST-9999'
		AND data->>'amount' = '77' AND data ? 'duplicate_of_charge' AND data->>'receipt' IS NOT NULL`, *fwd.TransactionID); n != 1 {
		t.Fatal("payment.received payload")
	}
	// Unknown prefix: unmatched, no event.
	unk := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 70, "WHO-1", testShortCode))
	if unk.TransactionID == nil || e.txn(*unk.TransactionID).AppID != nil || e.txn(*unk.TransactionID).Status != store.TxnStatusSucceeded ||
		e.count(`SELECT count(*) FROM events WHERE subject_id = $1`, *unk.TransactionID) != 0 {
		t.Fatalf("unknown: %+v", unk)
	}
	// Another short code: not recorded.
	before := e.count(`SELECT count(*) FROM transactions`)
	foreign := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 70, c.AccountNumber, "600000"))
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

	// Suspended app (rule 8): still recorded, paid and forwarded, with a warning.
	c2 := e.charge(app, 60)
	if _, err := e.apps.SetStatus(e.ctx, operator, app, store.AppStatusSuspended); err != nil {
		t.Fatal(err)
	}
	in := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 60, c2.AccountNumber, testShortCode))
	if in.Error != nil || in.TransactionID == nil || *e.txn(*in.TransactionID).AppID != app ||
		e.chargeState(app, c2.ID).Status != store.ChargeStatusSucceeded {
		t.Fatalf("inactive: %+v", in)
	}
	if !strings.Contains(e.logs.String(), "inactive app") {
		t.Fatal("no inactive warning")
	}
	e.checkInvariants()
	e.assertNoLeak()
}

// An app-supplied account number may repeat: a paybill payment pays a charge
// only when exactly one live charge matches account number AND amount.
func TestC2BAmbiguousAccountIsForwarded(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	q := store.New(e.db.App)
	a1 := e.chargeWithAccount(app, 500, "HOST-1042")
	a2 := e.chargeWithAccount(app, 500, "HOST-1042")
	b := e.chargeWithAccount(app, 800, "HOST-1042")

	// 500 matches two live charges: forwarded, neither paid.
	amb := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 500, "host 1042", testShortCode))
	if amb.TransactionID == nil || e.txn(*amb.TransactionID).ChargeID != nil || e.events(*amb.TransactionID, events.PaymentReceived) != 1 {
		t.Fatalf("ambiguous: %+v", amb)
	}
	for _, c := range []store.Charge{a1, a2, b} {
		if e.chargeState(app, c.ID).Status != store.ChargeStatusPending {
			t.Fatal("ambiguous payment paid a charge")
		}
	}
	// 800 matches exactly one: pays it.
	one := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 800, "HOST1042", testShortCode))
	if one.TransactionID == nil || e.chargeState(app, b.ID).Status != store.ChargeStatusSucceeded {
		t.Fatalf("single match: %+v", one)
	}
	// Cancel one of the 500s: now exactly one live charge matches.
	if _, err := q.CancelCharge(e.ctx, store.CancelChargeParams{AppID: app, ID: a1.ID}); err != nil {
		t.Fatal(err)
	}
	last := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(newReceipt(), 500, "HOST-1042", testShortCode))
	if last.TransactionID == nil || e.chargeState(app, a2.ID).Status != store.ChargeStatusSucceeded ||
		e.chargeState(app, a1.ID).Status != store.ChargeStatusCancelled {
		t.Fatalf("after cancel: %+v", last)
	}
	e.checkInvariants()
}

// A paybill notice for a waiting prompt completes that prompt; the STK
// callback with the same receipt is then a duplicate (one receipt, once).
func TestC2BCompletesWaitingPrompt(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 640)
	p := e.send(app, c.ID, custPhone)
	receipt := newReceipt()
	cb := e.deliver(store.MpesaCallbackKindC2bConfirmation, c2b(receipt, 640, c.AccountNumber, testShortCode))
	if cb.Error == nil || !strings.Contains(*cb.Error, "completed STK push") || *cb.TransactionID != p.TransactionID {
		t.Fatalf("c2b: %+v", cb)
	}
	if e.push(p.ID).Status != store.StkPushStatusSucceeded || e.chargeState(app, c.ID).Status != store.ChargeStatusSucceeded {
		t.Fatal("prompt not completed")
	}
	stkcb := e.deliver(store.MpesaCallbackKindStk, stkSuccess(*p.CheckoutRequestID, receipt, 640, custPhone))
	if stkcb.Error != nil || *stkcb.TransactionID != p.TransactionID || e.count(`SELECT count(*) FROM transactions WHERE provider_transaction_id = $1`, receipt) != 1 {
		t.Fatalf("stk after c2b: %+v", stkcb)
	}
	// The customer then also pays the prompt itself (another receipt): a
	// second payment, reported as a duplicate of the charge.
	second := e.deliver(store.MpesaCallbackKindStk, stkSuccess(*p.CheckoutRequestID, newReceipt(), 640, custPhone))
	if second.TransactionID == nil || *second.TransactionID == p.TransactionID {
		t.Fatalf("second: %+v", second)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE subject_id = $1 AND type = 'payment.received' AND data->>'duplicate_of_charge' = $2`,
		*second.TransactionID, c.ID.String()); n != 1 {
		t.Fatal("second payment not reported as duplicate_of_charge")
	}
	e.checkInvariants()
}

func TestSuccessCallbackWithoutReceipt(t *testing.T) {
	e := newEnv(t)
	app := e.app("HOST")
	c := e.charge(app, 100)
	p := e.send(app, c.ID, custPhone)
	raw := `{"Body":{"stkCallback":{"MerchantRequestID":"m","CheckoutRequestID":"` + *p.CheckoutRequestID +
		`","ResultCode":0,"ResultDesc":"ok","CallbackMetadata":{"Item":[{"Name":"Amount","Value":100}]}}}}`
	cb := e.deliver(store.MpesaCallbackKindStk, raw)
	if cb.Error == nil {
		t.Fatal("no error recorded")
	}
	if e.txn(p.TransactionID).Status != store.TxnStatusNeedsReview || e.push(p.ID).Status != store.StkPushStatusUnknown ||
		e.chargeState(app, c.ID).Status != store.ChargeStatusNeedsReview {
		t.Fatal("not sent to review")
	}
	e.checkInvariants()
}
