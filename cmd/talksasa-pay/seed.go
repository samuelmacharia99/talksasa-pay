package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/accountref"
	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// demoState is the state a seeded charge is left in.
type demoState int

const (
	demoPending      demoState = iota
	demoPromptFailed           // pending, with one failed prompt (the customer cancelled)
	demoPaidSTK                // succeeded by an STK push with a receipt
	demoPaidC2B                // succeeded by a paybill payment to its account number
	demoNeedsReview            // a status check said paid but no receipt arrived
	demoExpired
	demoCancelled
)

type demoCharge struct {
	prefix, ref, desc string
	amount            int64
	state             demoState
}

// The demo data set: three apps (one suspended), a dozen charges in every
// state, a paybill payment forwarded as payment.received, one for the
// suspended app and one unmatched payment (unknown prefix).
var (
	demoApps = []struct {
		name, prefix string
		suspended    bool
	}{
		{"ISP Billing", "BILL", false},
		{"Cloud Hosting", "HOST", false},
		{"Legacy SMS", "SMS", true},
	}
	demoCharges = []demoCharge{
		{"BILL", "INV-1001", "Fibre 20 Mbps, December", 2500, demoPaidSTK},
		{"BILL", "INV-1002", "Fibre 40 Mbps, December", 4000, demoPaidC2B},
		{"BILL", "INV-1003", "Fibre 20 Mbps, December", 2500, demoPending},
		{"BILL", "INV-1004", "Router replacement", 3500, demoPromptFailed},
		{"BILL", "INV-1005", "Fibre 10 Mbps, November", 1500, demoExpired},
		{"BILL", "INV-1006", "Installation fee", 5000, demoCancelled},
		{"HOST", "HST-2001", "VPS small, 1 month", 1200, demoPaidSTK},
		{"HOST", "HST-2002", "Domain renewal example.co.ke", 1000, demoNeedsReview},
		{"HOST", "HST-2003", "VPS medium, 3 months", 7500, demoPending},
		{"HOST", "HST-2004", "Shared hosting, 1 year", 6000, demoPaidC2B},
		{"HOST", "HST-2005", "SSL certificate", 2000, demoExpired},
		{"HOST", "HST-2006", "Email plan upgrade", 800, demoCancelled},
	}
)

const demoPhone = "254708374149" // Safaricom's sandbox test number

// seedDemo fills a DEVELOPMENT database with demo data so the admin panel
// and pay page have something to show. Apps go through the app service
// (audited); charges and payments are written with the ledger helpers and
// store queries in one transaction each, so every database guard applies
// (receipt once, exact amount, legal status moves). It emits no events or
// webhooks. Safe to re-run: charges already seeded are left alone.
func seedDemo(ctx context.Context, a *services, actor audit.Actor, w io.Writer) error {
	apps := map[string]app.App{}
	for _, da := range demoApps {
		p, err := a.apps.Resolve(ctx, da.prefix)
		if errors.Is(err, app.ErrNotFound) {
			c, cerr := a.apps.Create(ctx, actor, app.CreateParams{Name: da.name, AccountPrefix: da.prefix})
			if cerr != nil {
				return cerr
			}
			p = c.App
			fmt.Fprintf(w, "created app %s (%s); its webhook secret was discarded, rotate it to get one\n", p.Name, p.AccountPrefix)
		} else if err != nil {
			return err
		}
		if da.suspended && p.Status == store.AppStatusActive {
			if p, err = a.apps.SetStatus(ctx, actor, p.ID, store.AppStatusSuspended); err != nil {
				return err
			}
			fmt.Fprintf(w, "suspended app %s\n", p.AccountPrefix)
		}
		apps[da.prefix] = p
	}

	q := store.New(a.pool)
	if _, err := q.GetChargeByIdempotencyKey(ctx, store.GetChargeByIdempotencyKeyParams{
		AppID: apps["BILL"].ID, IdempotencyKey: seedKey(demoCharges[0].ref),
	}); err == nil {
		fmt.Fprintln(w, "\nDemo charges already seeded; nothing more to do.")
		return nil
	} else if !db.IsNotFound(err) {
		return err
	}

	err := db.WithTx(ctx, a.pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		for _, dc := range demoCharges {
			if err := seedCharge(ctx, q, a.events, apps[dc.prefix], dc); err != nil {
				return fmt.Errorf("seed %s: %w", dc.ref, err)
			}
		}
		// Expire the charges seeded with a past expiry (the job's query).
		if _, err := q.ExpireDueCharges(ctx, 100); err != nil {
			return err
		}
		// Paybill money with no waiting charge: forwarded to the app as
		// payment.received (the app decides what it pays). Money for a
		// suspended app is still recorded and forwarded (money rule 8).
		for _, f := range []struct {
			prefix, account string
			amount          int64
		}{{"HOST", "HOST-9ZZZ", 1750}, {"SMS", "SMS-1042", 300}} {
			id := apps[f.prefix].ID
			txn, err := seedC2B(ctx, q, &id, nil, f.account, f.amount)
			if err != nil {
				return err
			}
			if err := a.events.EmitPaymentReceived(ctx, q, id, txn.ID, nil); err != nil {
				return err
			}
		}
		// Unknown prefix: held unmatched until staff assign it.
		_, err := seedC2B(ctx, q, nil, nil, "ACME-77", 4200)
		return err
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\nSeeded %d demo charges (pending, prompt failed, paid by STK and paybill, needs review, expired, cancelled),\n"+
		"a forwarded paybill payment, a payment for the suspended app and one unmatched payment (account ACME-77).\n"+
		"Assign it with: talksasa-pay admin txn assign --txn <id> --app BILL --reason \"...\" --actor NAME\n", len(demoCharges))
	return nil
}

func seedKey(ref string) string { return "seed-demo-" + ref }

func seedCharge(ctx context.Context, q *store.Queries, em *events.Emitter, ap app.App, dc demoCharge) error {
	acct, err := accountref.Generate(ap.AccountPrefix)
	if err != nil {
		return err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return err
	}
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return err
	}
	expires := time.Now().Add(24 * time.Hour)
	if dc.state == demoExpired {
		expires = time.Now().Add(-time.Hour)
	}
	var ph *string
	if dc.state != demoPaidC2B {
		p := demoPhone
		ph = &p
	}
	desc := dc.desc
	hash := sha256.Sum256([]byte(dc.ref))
	c, err := q.InsertCharge(ctx, store.InsertChargeParams{
		ID: id, AppID: ap.ID, Reference: dc.ref, Description: &desc, Amount: dc.amount,
		Metadata:      []byte(`{"source":"seed demo","invoice":"` + dc.ref + `"}`),
		AccountNumber: acct, AccountNumberNorm: accountref.Normalize(acct), AccountGenerated: true, Phone: ph,
		HostedToken:    strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(tok)),
		IdempotencyKey: seedKey(dc.ref), RequestHash: hash[:], ExpiresAt: expires,
	})
	if err != nil {
		return err
	}

	switch dc.state {
	case demoPending, demoExpired:
		return nil
	case demoCancelled:
		_, err = q.CancelCharge(ctx, store.CancelChargeParams{AppID: ap.ID, ID: c.ID})
		return err
	case demoPaidC2B:
		txn, err := seedC2B(ctx, q, &ap.ID, &c.ID, c.AccountNumber, c.Amount)
		if err != nil {
			return err
		}
		_, err = ledger.SucceedCharge(ctx, q, em, c.ID, txn.ID)
		return err
	}

	// The rest start with a prompt.
	txn, err := ledger.StartSTK(ctx, q, ledger.StartSTKParams{
		AppID: ap.ID, ChargeID: c.ID, Amount: money.KES(c.Amount), Phone: demoPhone, AccountNumber: c.AccountNumber,
	})
	if err != nil {
		return err
	}
	pushID, err := uuid.NewV7()
	if err != nil {
		return err
	}
	if _, err := q.InsertSTKPush(ctx, store.InsertSTKPushParams{
		ID: pushID, AppID: ap.ID, ChargeID: c.ID, TransactionID: txn.ID, Source: "pay_page", Phone: demoPhone, Amount: c.Amount,
	}); err != nil {
		return err
	}
	checkout := "ws_CO_SEED_" + strings.ReplaceAll(dc.ref, "-", "")
	merchant := "SEED-" + dc.ref
	if _, err := q.MarkSTKPushSent(ctx, store.MarkSTKPushSentParams{
		MerchantRequestID: &merchant, CheckoutRequestID: &checkout, ID: pushID,
	}); err != nil {
		return err
	}
	finish := func(st store.StkPushStatus, code, desc string) error {
		_, err := q.FinishSTKPush(ctx, store.FinishSTKPushParams{Status: st, ResultCode: &code, ResultDesc: &desc, ID: pushID})
		return err
	}

	switch dc.state {
	case demoPromptFailed:
		if _, err := ledger.FailSTK(ctx, q, txn.ID, "1032", "Request cancelled by user", nil); err != nil {
			return err
		}
		return finish(store.StkPushStatusFailed, "1032", "Request cancelled by user")
	case demoNeedsReview:
		if _, err := ledger.MarkNeedsReview(ctx, q, txn.ID, "0", "Status check reported success but no receipt arrived"); err != nil {
			return err
		}
		if err := finish(store.StkPushStatusUnknown, "0", "The service request is processed successfully."); err != nil {
			return err
		}
		_, err = q.MarkChargeNeedsReview(ctx, c.ID)
		return err
	case demoPaidSTK:
		paid, err := ledger.CompleteSTK(ctx, q, ledger.CompleteSTKParams{
			TransactionID: txn.ID, Receipt: seedReceipt(), RawPayload: []byte(`{"source":"seed demo"}`),
		})
		if err != nil {
			return err
		}
		if err := finish(store.StkPushStatusSucceeded, "0", "The service request is processed successfully."); err != nil {
			return err
		}
		_, err = ledger.SucceedCharge(ctx, q, em, c.ID, paid.ID)
		return err
	}
	return fmt.Errorf("unknown demo state %d", dc.state)
}

// seedC2B records a succeeded paybill payment.
func seedC2B(ctx context.Context, q *store.Queries, appID, chargeID *uuid.UUID, account string, amount int64) (store.Transaction, error) {
	r := seedReceipt()
	txn, _, err := ledger.RecordIncoming(ctx, q, ledger.IncomingPayment{
		Channel: store.TxnChannelMpesaC2b, Receipt: r, Amount: money.KES(amount), Phone: demoPhone,
		AccountNumber: account, AppID: appID, ChargeID: chargeID,
		RawPayload: []byte(`{"TransID":"` + r + `","BillRefNumber":"` + account + `","source":"seed demo"}`),
	})
	return txn, err
}

// seedReceipt returns a unique M-Pesa-style receipt ("SD" + 8 random
// base32 characters), so re-seeding a fresh database never collides.
func seedReceipt() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return "SD" + base32.StdEncoding.EncodeToString(b)
}

