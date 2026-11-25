package integrationtest

// One test per money rule of the gateway plan (approved 2026-11-25), plus
// staff assignment and reversal, through the real services: charge.Service,
// stk.Service (callbacks) and ledger.Service. Each ends with
// mpesaInvariants.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/accountref"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/charge"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// create creates a charge through the service and fails the test on error.
func (m *mpesaEnv) create(appID uuid.UUID, p charge.CreateParams) charge.Created {
	m.t.Helper()
	if p.IdempotencyKey == "" {
		p.IdempotencyKey = uuid.NewString()
	}
	if p.Reference == "" {
		p.Reference = "INV-" + uuid.NewString()[:8]
	}
	c, err := m.charges.Create(m.ctx, audit.App(appID), appID, p)
	if err != nil {
		m.t.Fatalf("create charge: %v", err)
	}
	return c
}

// stkPay delivers a success callback for push p (exact amount) and returns
// the receipt.
func (m *mpesaEnv) stkPay(p stk.Push, amount int64) string {
	m.t.Helper()
	receipt := newMpesaReceipt()
	m.mustDeliver(store.MpesaCallbackKindStk, stkSuccessBody(*p.CheckoutRequestID, receipt, amount, p.Phone))
	return receipt
}

// c2bPay delivers a paybill confirmation and returns its transaction.
func (m *mpesaEnv) c2bPay(account string, amount int64) store.Transaction {
	m.t.Helper()
	id := m.mustDeliver(store.MpesaCallbackKindC2bConfirmation, c2bBody(newMpesaReceipt(), amount, account))
	var txn *uuid.UUID
	if err := m.db.Owner.QueryRow(m.ctx, `SELECT transaction_id FROM mpesa_callbacks WHERE id = $1`, id).Scan(&txn); err != nil {
		m.t.Fatal(err)
	}
	if txn == nil {
		m.t.Fatal("paybill payment not recorded")
	}
	return m.txn(*txn)
}

// Rule 1: amount 1..250,000, never changes; reference unique per app for
// ever; metadata a JSON object <= 4 KB echoed back; expires_in 15 min..7 days
// (default 24 h); Create is idempotent on the key.
func TestMpesaRule1ChargeCreate(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	app := m.app("RULA").App.ID
	other := m.app("RULB").App.ID
	act := audit.App(app)
	base := func() charge.CreateParams {
		return charge.CreateParams{Reference: "INV-1", Amount: 1500, Description: "Hosting, November", IdempotencyKey: uuid.NewString()}
	}
	bad := map[string]struct {
		mut  func(*charge.CreateParams)
		want error
	}{
		"amount 0":          {func(p *charge.CreateParams) { p.Amount = 0 }, charge.ErrInvalidAmount},
		"amount too large":  {func(p *charge.CreateParams) { p.Amount = 250_001 }, charge.ErrInvalidAmount},
		"no reference":      {func(p *charge.CreateParams) { p.Reference = "  " }, charge.ErrInvalidReference},
		"long reference":    {func(p *charge.CreateParams) { p.Reference = strings.Repeat("r", 129) }, charge.ErrInvalidReference},
		"blank description": {func(p *charge.CreateParams) { p.Description = "   " }, charge.ErrInvalidDescription},
		"long description":  {func(p *charge.CreateParams) { p.Description = strings.Repeat("d", 501) }, charge.ErrInvalidDescription},
		"metadata array":    {func(p *charge.CreateParams) { p.Metadata = json.RawMessage(`[1]`) }, charge.ErrInvalidMetadata},
		"metadata too big": {func(p *charge.CreateParams) {
			p.Metadata = json.RawMessage(`{"x":"` + strings.Repeat("a", 4100) + `"}`)
		}, charge.ErrInvalidMetadata},
		"expiry too short":   {func(p *charge.CreateParams) { p.ExpiresIn = 14 * time.Minute }, charge.ErrInvalidExpiry},
		"expiry too long":    {func(p *charge.CreateParams) { p.ExpiresIn = 8 * 24 * time.Hour }, charge.ErrInvalidExpiry},
		"bad phone":          {func(p *charge.CreateParams) { p.Phone = "0612" }, charge.ErrInvalidPhone},
		"no idempotency key": {func(p *charge.CreateParams) { p.IdempotencyKey = "" }, charge.ErrInvalidIdempotency},
		"wrong prefix":       {func(p *charge.CreateParams) { p.AccountNumber = "RULB-1042" }, charge.ErrInvalidAccountNumber},
		"no digit":           {func(p *charge.CreateParams) { p.AccountNumber = "RULA-X1" }, charge.ErrInvalidAccountNumber},
		"account too long":   {func(p *charge.CreateParams) { p.AccountNumber = "RULA-12345678" }, charge.ErrInvalidAccountNumber},
	}
	for name, c := range bad {
		p := base()
		c.mut(&p)
		if _, err := m.charges.Create(m.ctx, act, app, p); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}

	p := base()
	p.Metadata = json.RawMessage(`{ "invoice": 1042, "customer": "c-9" }`)
	created := m.create(app, p)
	c := created.Charge
	if c.Status != store.ChargeStatusPending || c.Amount != 1500 || c.Reference != "INV-1" || c.Currency != "KES" ||
		len(c.HostedToken) < 40 || created.Push != nil || created.PushErr != nil || created.Replayed {
		t.Fatalf("created: %+v", created)
	}
	if d := c.ExpiresAt.Sub(c.CreatedAt); d < 23*time.Hour || d > 25*time.Hour {
		t.Fatalf("default expiry %v", d)
	}
	var meta map[string]any
	if err := json.Unmarshal(c.Metadata, &meta); err != nil || meta["invoice"] != float64(1042) {
		t.Fatalf("metadata %s", c.Metadata)
	}
	// Idempotent replay; a different body with the same key conflicts.
	again, err := m.charges.Create(m.ctx, act, app, p)
	if err != nil || !again.Replayed || again.Charge.ID != c.ID {
		t.Fatalf("replay: %+v %v", again, err)
	}
	p2 := p
	p2.Amount = 1600
	if _, err := m.charges.Create(m.ctx, act, app, p2); !errors.Is(err, charge.ErrIdempotencyConflict) {
		t.Fatalf("conflict: %v", err)
	}
	// Same key, many at once: one charge.
	key := uuid.NewString()
	ids := make([]uuid.UUID, 20)
	requireNoErrors(t, "concurrent create", storm(20, func(i int) error {
		r, err := m.charges.Create(m.ctx, act, app, charge.CreateParams{Reference: "INV-C", Amount: 10, IdempotencyKey: key})
		ids[i] = r.Charge.ID
		return err
	}))
	for _, id := range ids {
		if id != ids[0] {
			t.Fatal("same key created two charges")
		}
	}
	// Reference unique per app for ever (also after a cancel); other apps
	// may reuse it.
	if _, err := m.charges.Cancel(m.ctx, act, app, c.ID); err != nil {
		t.Fatal(err)
	}
	dup := base()
	if _, err := m.charges.Create(m.ctx, act, app, dup); !errors.Is(err, charge.ErrDuplicateReference) {
		t.Fatalf("reused reference: %v", err)
	}
	m.create(other, base())
	// Get / List are app-scoped.
	if _, err := m.charges.Get(m.ctx, other, c.ID); !errors.Is(err, charge.ErrNotFound) {
		t.Fatalf("get other app: %v", err)
	}
	if got, err := m.charges.Get(m.ctx, app, c.ID); err != nil || got.Status != store.ChargeStatusCancelled {
		t.Fatalf("get: %+v %v", got, err)
	}
	st := store.ChargeStatusCancelled
	if ls, err := m.charges.List(m.ctx, app, charge.ListParams{Status: &st}); err != nil || len(ls) != 1 || ls[0].ID != c.ID {
		t.Fatalf("list cancelled: %v %v", ls, err)
	}
	if ls, err := m.charges.List(m.ctx, app, charge.ListParams{Reference: "INV-C"}); err != nil || len(ls) != 1 {
		t.Fatalf("list by reference: %v %v", ls, err)
	}
	all, err := m.charges.List(m.ctx, app, charge.ListParams{Limit: 1})
	if err != nil || len(all) != 1 {
		t.Fatalf("list page: %v %v", all, err)
	}
	next, err := m.charges.List(m.ctx, app, charge.ListParams{Limit: 10, CursorAt: &all[0].CreatedAt, CursorID: &all[0].ID})
	if err != nil || len(next) != 1 || next[0].ID == all[0].ID {
		t.Fatalf("list next page: %v %v", next, err)
	}
	bogus := store.ChargeStatus("paid")
	if _, err := m.charges.List(m.ctx, app, charge.ListParams{Status: &bogus}); !errors.Is(err, charge.ErrInvalidStatus) {
		t.Fatalf("bad status: %v", err)
	}
	if _, err := m.charges.List(m.ctx, app, charge.ListParams{CursorID: &c.ID}); !errors.Is(err, charge.ErrInvalidCursor) {
		t.Fatalf("half cursor: %v", err)
	}
	// The amount never changes (database guard).
	if _, err := m.db.App.Exec(m.ctx, `UPDATE charges SET amount = 1 WHERE id = $1`, c.ID); err == nil {
		t.Fatal("charge amount changed")
	}
	// A suspended app cannot create charges.
	if _, err := m.apps.SetStatus(m.ctx, operator, other, store.AppStatusSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err := m.charges.Create(m.ctx, audit.App(other), other, charge.CreateParams{Reference: "X", Amount: 5, IdempotencyKey: "k"}); !errors.Is(err, charge.ErrAppNotActive) {
		t.Fatalf("suspended: %v", err)
	}
	if n := m.count(`SELECT count(*) FROM audit_log WHERE action = 'charge.created' AND entity_id = $1`, c.ID.String()); n != 1 {
		t.Fatalf("create audited %d times", n)
	}
	m.mpesaInvariants()
}

// Rule 2: every charge has an account number, the app's own (its prefix)
// or generated (12 characters with a check character); it is the STK
// AccountReference and what the paybill matches on.
func TestMpesaRule2AccountNumber(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	app := m.app("ACCT").App.ID
	gen := m.create(app, charge.CreateParams{Amount: 100}).Charge
	if !gen.AccountGenerated || len(gen.AccountNumber) != accountref.MaxLen || !strings.HasPrefix(gen.AccountNumber, "ACCT-") ||
		!accountref.ValidCheck("ACCT", gen.AccountNumber) || gen.AccountNumberNorm != accountref.Normalize(gen.AccountNumber) {
		t.Fatalf("generated: %+v", gen)
	}
	own := m.create(app, charge.CreateParams{Amount: 100, AccountNumber: " acct-1042 "}).Charge
	if own.AccountGenerated || own.AccountNumber != "ACCT-1042" {
		t.Fatalf("app-supplied: %+v", own)
	}
	// App-supplied numbers may repeat across charges.
	own2 := m.create(app, charge.CreateParams{Amount: 200, AccountNumber: "ACCT-1042"}).Charge
	p := m.mustSend(app, own2.ID, newPhone(0))
	if _, req := m.client.last(); req.AccountReference != "ACCT-1042" || req.Amount != 200 {
		t.Fatalf("STK request: %+v", req)
	}
	m.stkPay(p, 200)
	m.assertCharge(own2.ID, store.ChargeStatusSucceeded)
	// A typed generated number (any case, spaces, no hyphen) pays its charge.
	typed := strings.ToLower(strings.Replace(gen.AccountNumber, "-", " ", 1))
	txn := m.c2bPay(typed, 100)
	if txn.ChargeID == nil || *txn.ChargeID != gen.ID {
		t.Fatalf("paybill with %q: %+v", typed, txn)
	}
	m.assertCharge(gen.ID, store.ChargeStatusSucceeded)
	m.mpesaInvariants()
}

// Rule 3: succeeded only on a receipt for exactly the charge amount; each
// receipt once. SucceedCharge refuses a payment of another amount: the
// charge stays unpaid and the money is forwarded (payment.received).
func TestMpesaRule3ReceiptExactAmountOnce(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	app := m.app("EXACT").App.ID
	c := m.create(app, charge.CreateParams{Amount: 1000}).Charge
	var res ledger.SucceedResult
	err := db.WithTx(m.ctx, m.db.App, worker.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		txn, _, err := ledger.RecordIncoming(ctx, q, ledger.IncomingPayment{
			Channel: store.TxnChannelMpesaC2b, Receipt: newMpesaReceipt(), Amount: money.KES(999), AppID: &app, ChargeID: &c.ID,
			AccountNumber: c.AccountNumber,
		})
		if err != nil {
			return err
		}
		res, err = ledger.SucceedCharge(ctx, q, m.events, c.ID, txn.ID)
		return err
	})
	if err != nil || !res.AmountMismatch || res.Succeeded {
		t.Fatalf("mismatch: %+v %v", res, err)
	}
	m.assertCharge(c.ID, store.ChargeStatusPending)
	// The database refuses to succeed a charge with the wrong amount anyway.
	if _, err := m.db.App.Exec(m.ctx, `UPDATE charges SET status = 'succeeded', succeeded_at = now(),
		transaction_id = (SELECT id FROM transactions WHERE charge_id = $1) WHERE id = $1`, c.ID); err == nil {
		t.Fatal("charges_guard let a wrong amount pay the charge")
	}
	// The exact amount pays it; the same receipt again changes nothing.
	p := m.mustSend(app, c.ID, newPhone(0))
	receipt := m.stkPay(p, 1000)
	m.mustDeliver(store.MpesaCallbackKindC2bConfirmation, c2bBody(receipt, 1000, c.AccountNumber))
	if m.receiptRows(receipt) != 1 {
		t.Fatal("receipt recorded twice")
	}
	m.assertCharge(c.ID, store.ChargeStatusSucceeded)
	m.mpesaInvariants()
}

// Rule 4: Create with a phone sends the first prompt; a failed prompt does
// not fail the charge; it can be prompted again, one live prompt at a time.
func TestMpesaRule4Prompts(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	app := m.app("PRMT").App.ID
	ph := newPhone(0)
	created := m.create(app, charge.CreateParams{Amount: 300, Phone: "0" + ph[3:]})
	if created.Push == nil || created.PushErr != nil || created.Push.Status != store.StkPushStatusSent || created.Push.Phone != ph {
		t.Fatalf("first prompt: %+v", created)
	}
	c := created.Charge
	// Replay returns the same prompt and does not prompt again.
	before := m.client.pushCount()
	again, err := m.charges.Create(m.ctx, audit.App(app), app, charge.CreateParams{
		Reference: c.Reference, Amount: 300, Phone: ph, IdempotencyKey: c.IdempotencyKey,
	})
	if err != nil || !again.Replayed || again.Push == nil || again.Push.ID != created.Push.ID || m.client.pushCount() != before {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err := m.sendApp(app, c.ID, ph); !isInProgress(err) {
		t.Fatalf("second live prompt: %v", err)
	}
	m.mustDeliver(store.MpesaCallbackKindStk, stkFailureBody(*created.Push.CheckoutRequestID, "1032", "Request cancelled by user"))
	m.assertCharge(c.ID, store.ChargeStatusPending)
	if m.eventsFor(created.Push.ID, events.STKPushFailed) != 1 {
		t.Fatal("no stk_push.failed")
	}
	p2 := m.mustSend(app, c.ID, ph)
	m.stkPay(p2, 300)
	m.assertCharge(c.ID, store.ChargeStatusSucceeded)

	// Daraja refuses the first prompt: the charge is still created.
	m.client.setHook(func(context.Context, mpesa.STKPushRequest, string) error {
		return &mpesa.Error{Kind: mpesa.Rejected, Message: "Invalid PhoneNumber"}
	})
	rej := m.create(app, charge.CreateParams{Amount: 50, Phone: ph})
	if !errors.Is(rej.PushErr, stk.ErrDaraja) || rej.Push == nil || rej.Push.Status != store.StkPushStatusFailed {
		t.Fatalf("rejected first prompt: %+v", rej)
	}
	m.assertCharge(rej.Charge.ID, store.ChargeStatusPending)
	m.client.setHook(nil)
	// M-Pesa off: the charge is created, PushErr says why.
	off := charge.NewService(charge.Options{Pool: m.db.App, Events: m.events, Logger: m.logger})
	r, err := off.Create(m.ctx, audit.App(app), app, charge.CreateParams{Reference: "OFF-1", Amount: 70, Phone: ph, IdempotencyKey: "off-1"})
	if err != nil || !errors.Is(r.PushErr, stk.ErrUnavailable) || r.Push != nil {
		t.Fatalf("mpesa off: %+v %v", r, err)
	}
	m.mpesaInvariants()
}

// Rule 5: "success" without a receipt -> needs_review (transaction and
// charge); staff enter the receipt or fail it (audited). A charge in
// needs_review never expires; failed, it goes back to pending and expires
// later.
func TestMpesaRule5NeedsReview(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	app := m.app("REVW").App.ID
	review := func(expiresIn time.Duration) (store.Charge, stk.Push) {
		c := m.create(app, charge.CreateParams{Amount: 400, ExpiresIn: expiresIn}).Charge
		p := m.mustSend(app, c.ID, newPhone(2))
		m.client.setQuery(*p.CheckoutRequestID, mpesa.STKQueryResult{State: mpesa.QuerySucceeded, ResultCode: "0"})
		m.advance(2 * time.Minute)
		if r, err := m.svc.Sweep(m.ctx, 10); err != nil || r.NeedsReview != 1 {
			t.Fatalf("sweep: %+v %v", r, err)
		}
		m.assertCharge(c.ID, store.ChargeStatusNeedsReview)
		if m.eventsFor(c.ID, events.ChargeNeedsReview) != 1 || m.eventsFor(p.ID, events.STKPushNeedsReview) != 1 {
			t.Fatal("needs_review events")
		}
		return c, p
	}
	c, p := review(0)
	receipt := newMpesaReceipt()
	if _, err := m.svc.Resolve(m.ctx, mpesaStaff(), stk.ResolveParams{TransactionID: p.TransactionID, Confirm: true, Receipt: receipt, Reason: "on the statement"}); err != nil {
		t.Fatal(err)
	}
	if got := m.assertCharge(c.ID, store.ChargeStatusSucceeded); *got.TransactionID != p.TransactionID {
		t.Fatal("confirm paid with another transaction")
	}

	// Past its expiry while in review: never expired; staff fail -> pending
	// -> the expiry job expires it.
	c2, p2 := review(15 * time.Minute)
	if _, err := m.db.Owner.Exec(m.ctx, `ALTER TABLE charges DISABLE TRIGGER charges_guard`); err != nil {
		t.Fatal(err)
	}
	_, err := m.db.Owner.Exec(m.ctx, `UPDATE charges SET expires_at = now() - interval '1 minute' WHERE id = $1`, c2.ID)
	if _, err2 := m.db.Owner.Exec(m.ctx, `ALTER TABLE charges ENABLE TRIGGER charges_guard`); err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if n, err := m.charges.Expire(m.ctx, 100); err != nil || n != 0 {
		t.Fatalf("expired a needs_review charge: %d %v", n, err)
	}
	if _, err := m.svc.Resolve(m.ctx, mpesaStaff(), stk.ResolveParams{TransactionID: p2.TransactionID, Reason: "not on the statement"}); err != nil {
		t.Fatal(err)
	}
	m.assertCharge(c2.ID, store.ChargeStatusPending)
	if n, err := m.charges.Expire(m.ctx, 100); err != nil || n != 1 {
		t.Fatalf("expire after review: %d %v", n, err)
	}
	m.assertCharge(c2.ID, store.ChargeStatusExpired)
	if m.eventsFor(c2.ID, events.ChargeExpired) != 1 || m.count(`SELECT count(*) FROM audit_log WHERE action = 'transaction.resolved'`) != 2 {
		t.Fatal("expiry event or resolve audit missing")
	}
	m.mpesaInvariants()
}

// Rule 6: late money is never dropped. Money for a cancelled or expired
// charge succeeds it; money for a succeeded charge is its own transaction,
// reported as payment.received with duplicate_of_charge.
func TestMpesaRule6LateMoney(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	app := m.app("LATE").App.ID
	act := audit.App(app)

	cancelled := m.create(app, charge.CreateParams{Amount: 500}).Charge
	if r, err := m.charges.Cancel(m.ctx, act, app, cancelled.ID); err != nil || r.AlreadyCancelled {
		t.Fatalf("cancel: %+v %v", r, err)
	}
	if r, err := m.charges.Cancel(m.ctx, act, app, cancelled.ID); err != nil || !r.AlreadyCancelled {
		t.Fatalf("cancel again: %+v %v", r, err)
	}
	if m.eventsFor(cancelled.ID, events.ChargeCancelled) != 1 {
		t.Fatal("charge.cancelled")
	}
	// A cancelled charge is no longer live, so a paybill payment does not
	// match it (forwarded); late money that names it (the prompt it had)
	// does succeed it.
	_ = m.c2bPay(cancelled.AccountNumber, 500)
	m.assertCharge(cancelled.ID, store.ChargeStatusCancelled)

	expiring := m.create(app, charge.CreateParams{Amount: 600}).Charge
	p := m.mustSend(app, expiring.ID, newPhone(0))
	m.client.setQuery(*p.CheckoutRequestID, mpesa.STKQueryResult{State: mpesa.QueryFailed, ResultCode: "1037"})
	m.advance(2 * time.Minute)
	if r, err := m.svc.Sweep(m.ctx, 10); err != nil || r.Failed != 1 {
		t.Fatalf("sweep: %+v %v", r, err)
	}
	if _, err := m.db.Owner.Exec(m.ctx, `ALTER TABLE charges DISABLE TRIGGER charges_guard`); err != nil {
		t.Fatal(err)
	}
	_, err := m.db.Owner.Exec(m.ctx, `UPDATE charges SET expires_at = now() - interval '1 minute' WHERE id = $1`, expiring.ID)
	if _, err2 := m.db.Owner.Exec(m.ctx, `ALTER TABLE charges ENABLE TRIGGER charges_guard`); err != nil || err2 != nil {
		t.Fatal(err, err2)
	}
	if n, err := m.charges.Expire(m.ctx, 100); err != nil || n != 1 {
		t.Fatalf("expire: %d %v", n, err)
	}
	m.assertCharge(expiring.ID, store.ChargeStatusExpired)
	m.stkPay(p, 600) // the prompt's money arrives after all
	m.assertCharge(expiring.ID, store.ChargeStatusSucceeded)
	if m.eventsFor(expiring.ID, events.ChargeExpired) != 1 || m.eventsFor(expiring.ID, events.ChargeSucceeded) != 1 {
		t.Fatal("expired then succeeded events")
	}

	// Paid twice: a second prompt payment after the charge succeeded.
	paid := m.create(app, charge.CreateParams{Amount: 700}).Charge
	ph := newPhone(0)
	p1 := m.mustSend(app, paid.ID, ph)
	m.mustDeliver(store.MpesaCallbackKindC2bConfirmation, c2bBody(newMpesaReceipt(), 700, paid.AccountNumber)) // completes p1
	m.stkPay(p1, 700)                                                                                          // and the customer approved the prompt too
	if n, sum := m.succeededFor(paid.ID); n != 2 || sum != 1400 {
		t.Fatalf("paid twice: %d payments totalling %d", n, sum)
	}
	if c := m.count(`SELECT count(*) FROM events WHERE type = 'payment.received' AND data->>'duplicate_of_charge' = $1`, paid.ID.String()); c != 1 {
		t.Fatalf("duplicate_of_charge events %d", c)
	}
	if m.eventsFor(paid.ID, events.ChargeSucceeded) != 1 {
		t.Fatal("charge succeeded twice")
	}
	m.mpesaInvariants()
}

// Rule 7: paybill routing by account-number prefix. Exactly one live charge
// with the account number and exact amount: paid. Prefix known otherwise:
// forwarded (payment.received with account number, amount, phone and
// receipt). Unknown prefix: unmatched, no event. Another paybill: not
// recorded.
func TestMpesaRule7Paybill(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	app := m.app("PAYB").App.ID
	c := m.create(app, charge.CreateParams{Amount: 900}).Charge
	if txn := m.c2bPay(c.AccountNumber, 900); txn.ChargeID == nil || *txn.ChargeID != c.ID {
		t.Fatalf("match: %+v", txn)
	}
	m.assertCharge(c.ID, store.ChargeStatusSucceeded)

	fwd := m.c2bPay("PAYB-77", 1234)
	if fwd.ChargeID != nil || *fwd.AppID != app {
		t.Fatalf("forward: %+v", fwd)
	}
	var data map[string]any
	if err := m.db.Owner.QueryRow(m.ctx, `SELECT data FROM events WHERE subject_id = $1 AND type = 'payment.received'`, fwd.ID).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if data["account_number"] != "PAYB-77" || data["amount"] != float64(1234) || data["receipt"] != *fwd.ProviderTransactionID ||
		data["object"] != "transaction" || data["duplicate_of_charge"] != nil {
		t.Fatalf("payment.received data: %v", data)
	}
	if _, ok := data["duplicate_of_charge"]; !ok {
		t.Fatal("duplicate_of_charge missing from payment.received")
	}

	unk := m.c2bPay("ZZZZ-1", 50)
	if unk.AppID != nil || m.count(`SELECT count(*) FROM events WHERE subject_id = $1`, unk.ID) != 0 {
		t.Fatalf("unknown prefix: %+v", unk)
	}
	before := m.count(`SELECT count(*) FROM transactions`)
	foreign := strings.Replace(c2bBody(newMpesaReceipt(), 50, "PAYB-77"), `"`+mpesaShortCode+`"`, `"600111"`, 1)
	m.mustDeliver(store.MpesaCallbackKindC2bConfirmation, foreign)
	if m.count(`SELECT count(*) FROM transactions`) != before || m.alarms("another short code") == 0 {
		t.Fatal("foreign short code recorded or not alarmed")
	}
	m.mpesaInvariants()
}

// Rule 8: money for an inactive app is still recorded and forwarded, with a
// warning.
func TestMpesaRule8InactiveApp(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	app := m.app("SUSP").App.ID
	c := m.create(app, charge.CreateParams{Amount: 300}).Charge
	p := m.mustSend(app, c.ID, newPhone(0))
	if _, err := m.apps.SetStatus(m.ctx, operator, app, store.AppStatusSuspended); err != nil {
		t.Fatal(err)
	}
	m.stkPay(p, 300)
	m.assertCharge(c.ID, store.ChargeStatusSucceeded)
	fwd := m.c2bPay("SUSP-5", 40)
	if *fwd.AppID != app || m.eventsFor(fwd.ID, events.PaymentReceived) != 1 {
		t.Fatalf("forward: %+v", fwd)
	}
	if !strings.Contains(m.logs.String(), "inactive app") {
		t.Fatal("no inactive-app warning")
	}
	m.mpesaInvariants()
}

// Rule 9: staff reversal: admin only, reason, audited; the charge becomes
// reversed and the app gets transaction.reversed. Reversing twice (also
// concurrently) is refused; only the paying transaction reverses the charge.
func TestMpesaRule9Reverse(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	app := m.app("REVS").App.ID
	c := m.create(app, charge.CreateParams{Amount: 800}).Charge
	pay := m.c2bPay(c.AccountNumber, 800)
	admin := audit.Admin(uuid.New())

	_, err := m.ledger.Reverse(m.ctx, audit.App(app), ledger.ReverseParams{OriginalID: pay.ID, Reason: "customer dispute"})
	if !errors.Is(err, ledger.ErrActorRequired) {
		t.Fatalf("app reversed: %v", err)
	}
	if _, err := m.ledger.Reverse(m.ctx, admin, ledger.ReverseParams{OriginalID: pay.ID, Reason: "no"}); !errors.Is(err, ledger.ErrReasonRequired) {
		t.Fatalf("short reason: %v", err)
	}
	if _, err := m.ledger.Reverse(m.ctx, admin, ledger.ReverseParams{OriginalID: uuid.New(), Reason: "missing"}); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("unknown txn: %v", err)
	}

	// Ten admins reverse at once: exactly one reversal.
	var won atomic.Int64
	errs := storm(10, func(int) error {
		_, err := m.ledger.Reverse(m.ctx, admin, ledger.ReverseParams{OriginalID: pay.ID, Reason: "Safaricom reversed it", ProviderReceipt: "rev" + pay.ID.String()[:6]})
		if errors.Is(err, ledger.ErrAlreadyReversed) {
			return nil
		}
		if err == nil {
			won.Add(1)
		}
		return err
	})
	requireNoErrors(t, "reverse", errs)
	if won.Load() != 1 {
		t.Fatalf("%d reversals succeeded", won.Load())
	}
	if n := m.count(`SELECT count(*) FROM transactions WHERE kind = 'reversal' AND reverses_transaction_id = $1`, pay.ID); n != 1 {
		t.Fatalf("%d reversals", n)
	}
	got := m.assertCharge(c.ID, store.ChargeStatusReversed)
	if *got.TransactionID != pay.ID {
		t.Fatal("reversed charge lost its payment")
	}
	var rev store.Transaction
	rev, err = store.New(m.db.App).GetReversalOf(m.ctx, &pay.ID)
	if err != nil || rev.Amount != 800 || rev.Direction != store.TxnDirectionOut || *rev.ChargeID != c.ID || *rev.AppID != app ||
		*rev.Reason != "Safaricom reversed it" || rev.ProviderTransactionID == nil {
		t.Fatalf("reversal: %+v %v", rev, err)
	}
	if m.eventsFor(rev.ID, events.TransactionReversed) != 1 ||
		m.count(`SELECT count(*) FROM events WHERE subject_id = $1 AND data->>'reverses_transaction_id' = $2 AND data->>'charge_id' = $3`,
			rev.ID, pay.ID.String(), c.ID.String()) != 1 {
		t.Fatal("transaction.reversed")
	}
	if m.count(`SELECT count(*) FROM audit_log WHERE action = 'transaction.reversed' AND entity_id = $1`, pay.ID.String()) != 1 {
		t.Fatal("reversal not audited once")
	}
	if _, err := m.ledger.Reverse(m.ctx, admin, ledger.ReverseParams{OriginalID: pay.ID, Reason: "again"}); !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Fatalf("second reversal: %v", err)
	}
	if _, err := m.ledger.Reverse(m.ctx, admin, ledger.ReverseParams{OriginalID: rev.ID, Reason: "reverse the reversal"}); !errors.Is(err, ledger.ErrNotReversible) {
		t.Fatalf("reversal of a reversal: %v", err)
	}

	// A second payment of a succeeded charge: reversing it leaves the charge
	// alone (it did not pay it).
	c2 := m.create(app, charge.CreateParams{Amount: 100}).Charge
	ph := newPhone(0)
	p := m.mustSend(app, c2.ID, ph)
	m.mustDeliver(store.MpesaCallbackKindC2bConfirmation, c2bBody(newMpesaReceipt(), 100, c2.AccountNumber))
	m.stkPay(p, 100)
	var second uuid.UUID
	if err := m.db.Owner.QueryRow(m.ctx, `SELECT t.id FROM transactions t JOIN charges c ON c.id = t.charge_id
		WHERE t.charge_id = $1 AND t.id <> c.transaction_id AND t.kind = 'payment'`, c2.ID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	res, err := m.ledger.Reverse(m.ctx, audit.CLI("ops"), ledger.ReverseParams{OriginalID: second, Reason: "refunded the double payment"})
	if err != nil || res.Charge != nil {
		t.Fatalf("reverse duplicate: %+v %v", res, err)
	}
	m.assertCharge(c2.ID, store.ChargeStatusSucceeded)
	m.mpesaInvariants()
}

// Staff assign an unmatched payment to an app: set once, audited; it pays
// the app's single live charge with that account number and exact amount
// (charge.succeeded), else the app gets payment.received.
func TestMpesaAssignToApp(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	app := m.app("ASGN").App.ID
	staff := audit.Admin(uuid.New())

	// Unknown prefix at the time of payment: unmatched.
	stray := m.c2bPay("NEWAP-1042", 450)
	if stray.AppID != nil {
		t.Fatal("setup: not unmatched")
	}
	if _, err := m.ledger.AssignToApp(m.ctx, staff, stray.ID, app, "x"); !errors.Is(err, ledger.ErrReasonRequired) {
		t.Fatalf("short reason: %v", err)
	}
	if _, err := m.ledger.AssignToApp(m.ctx, staff, stray.ID, uuid.New(), "customer called"); !errors.Is(err, ledger.ErrAppNotFound) {
		t.Fatalf("unknown app: %v", err)
	}
	got, err := m.ledger.AssignToApp(m.ctx, staff, stray.ID, app, "customer called with the invoice")
	if err != nil || got.AppID == nil || *got.AppID != app || got.ChargeID != nil {
		t.Fatalf("assign: %+v %v", got, err)
	}
	if m.eventsFor(stray.ID, events.PaymentReceived) != 1 ||
		m.count(`SELECT count(*) FROM audit_log WHERE action = 'transaction.assigned' AND entity_id = $1`, stray.ID.String()) != 1 {
		t.Fatal("assign: event or audit missing")
	}
	if _, err := m.ledger.AssignToApp(m.ctx, staff, stray.ID, app, "again please"); !errors.Is(err, ledger.ErrAlreadyAssigned) {
		t.Fatalf("assign twice: %v", err)
	}

	// The payment named an account number that matches exactly one live
	// charge of the app: assigning pays it. Concurrent assigns: one wins.
	c := m.create(app, charge.CreateParams{Amount: 350, AccountNumber: "ASGN-2042"}).Charge
	// (A paybill payment with a known prefix matches on arrival...)
	if stray2 := m.c2bPay("ASGN 2042", 350); stray2.ChargeID == nil {
		t.Fatalf("paybill did not pay the charge: %+v", stray2)
	}
	// ...so assignment matters for money recorded without an app, e.g. an
	// unmatched payment whose prefix staff recognise.
	c3 := m.create(app, charge.CreateParams{Amount: 275, AccountNumber: "ASGN-3042"}).Charge
	var unmatched store.Transaction
	if err := db.WithTx(m.ctx, m.db.App, worker.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		unmatched, _, err = ledger.RecordIncoming(ctx, q, ledger.IncomingPayment{
			Channel: store.TxnChannelMpesaC2b, Receipt: newMpesaReceipt(), Amount: 275, AccountNumber: "asgn-3042",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int64
	errs := storm(6, func(int) error {
		_, err := m.ledger.AssignToApp(m.ctx, staff, unmatched.ID, app, "statement shows ASGN-3042")
		if errors.Is(err, ledger.ErrAlreadyAssigned) {
			return nil
		}
		if err == nil {
			wins.Add(1)
		}
		return err
	})
	requireNoErrors(t, "assign", errs)
	if wins.Load() != 1 {
		t.Fatalf("%d assignments succeeded", wins.Load())
	}
	if n := m.count(`SELECT count(*) FROM audit_log WHERE action = 'transaction.assigned' AND entity_id = $1`, unmatched.ID.String()); n != 1 {
		t.Fatalf("assigned %d times", n)
	}
	got3 := m.assertCharge(c3.ID, store.ChargeStatusSucceeded)
	if *got3.TransactionID != unmatched.ID || m.eventsFor(c3.ID, events.ChargeSucceeded) != 1 {
		t.Fatal("assignment did not pay the matching charge")
	}
	m.assertCharge(c.ID, store.ChargeStatusSucceeded)
	// A pending (not succeeded) transaction cannot be assigned.
	pc := m.create(app, charge.CreateParams{Amount: 10}).Charge
	pp := m.mustSend(app, pc.ID, newPhone(0))
	if _, err := m.ledger.AssignToApp(m.ctx, staff, pp.TransactionID, app, "wrong row"); !errors.Is(err, ledger.ErrAlreadyAssigned) && !errors.Is(err, ledger.ErrNotAssignable) {
		t.Fatalf("assign a prompt's transaction: %v", err)
	}
	m.mpesaInvariants()
}
