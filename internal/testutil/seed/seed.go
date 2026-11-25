// Package seed writes charges and payments straight through the store
// queries, for tests that need data but do not test the charge service or
// the money core (internal/charge, ledger.SucceedCharge). It bypasses
// events and audit; the database guards still apply.
package seed

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/accountref"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

var seq atomic.Int64

// ChargeOpts customise Charge. Zero values pick sensible defaults.
type ChargeOpts struct {
	Reference     string    // default INV-<n>
	AccountNumber string    // default generated from the prefix
	Description   string    // default "Invoice"
	Phone         string    // normalised 2547XXXXXXXX, optional
	ExpiresAt     time.Time // default now + 24 h
	Metadata      []byte    // JSON object, optional
}

// HostedToken returns a fresh pay-page token (32 random bytes, lowercase
// unpadded base32: 52 characters).
func HostedToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// ChargeParams builds the insert for a pending charge of the app with
// prefix (goroutine-safe: no testing.T).
func ChargeParams(appID uuid.UUID, prefix string, amount int64, o ChargeOpts) (store.InsertChargeParams, error) {
	n := seq.Add(1)
	if o.Reference == "" {
		o.Reference = fmt.Sprintf("INV-%d-%d", time.Now().UnixNano()%1_000_000, n)
	}
	generated := o.AccountNumber == ""
	if generated {
		acct, err := accountref.Generate(prefix)
		if err != nil {
			return store.InsertChargeParams{}, err
		}
		o.AccountNumber = acct
	}
	if o.Description == "" {
		o.Description = "Invoice"
	}
	if o.ExpiresAt.IsZero() {
		o.ExpiresAt = time.Now().Add(24 * time.Hour)
	}
	var phone *string
	if o.Phone != "" {
		phone = &o.Phone
	}
	desc := o.Description
	hash := sha256.Sum256([]byte(o.Reference))
	id, err := uuid.NewV7()
	if err != nil {
		return store.InsertChargeParams{}, err
	}
	return store.InsertChargeParams{
		ID: id, AppID: appID, Reference: o.Reference, Description: &desc, Amount: amount,
		Metadata: o.Metadata, AccountNumber: o.AccountNumber, AccountNumberNorm: accountref.Normalize(o.AccountNumber),
		AccountGenerated: generated, Phone: phone, HostedToken: HostedToken(),
		IdempotencyKey: fmt.Sprintf("seed-%d-%s", n, uuid.NewString()), RequestHash: hash[:], ExpiresAt: o.ExpiresAt,
	}, nil
}

// Charge inserts a pending charge for the app with prefix.
func Charge(t testing.TB, ctx context.Context, q store.DBTX, appID uuid.UUID, prefix string, amount int64, o ChargeOpts) store.Charge {
	t.Helper()
	p, err := ChargeParams(appID, prefix, amount, o)
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.New(q).InsertCharge(ctx, p)
	if err != nil {
		t.Fatalf("seed charge: %v", err)
	}
	// Distinct created_at ordering even on fast machines.
	time.Sleep(2 * time.Millisecond)
	return c
}

// Receipt returns a fresh, valid M-Pesa-style receipt.
func Receipt() string { return fmt.Sprintf("SEED%06d", seq.Add(1)) }

// Payment records a succeeded paybill payment (no charge link) for appID,
// or unmatched when appID is nil.
func Payment(t testing.TB, ctx context.Context, pool db.Beginner, appID *uuid.UUID, account string, amount int64) store.Transaction {
	t.Helper()
	var txn store.Transaction
	err := db.WithTx(ctx, pool, "system:seed", func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		r := Receipt()
		txn, _, err = ledger.RecordIncoming(ctx, q, ledger.IncomingPayment{
			Channel: store.TxnChannelMpesaC2b, Receipt: r, Amount: money.KES(amount), Phone: "254712345678",
			AccountNumber: account, AppID: appID, RawPayload: []byte(`{"TransID":"` + r + `"}`),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return txn
}

// PayCharge records a succeeded paybill payment for exactly the charge's
// amount and marks the charge succeeded with it, in one transaction. It
// emits no events (the real path is ledger.SucceedCharge).
func PayCharge(t testing.TB, ctx context.Context, pool db.Beginner, c store.Charge) store.Transaction {
	t.Helper()
	var txn store.Transaction
	err := db.WithTx(ctx, pool, "system:seed", func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		var err error
		r := Receipt()
		txn, _, err = ledger.RecordIncoming(ctx, q, ledger.IncomingPayment{
			Channel: store.TxnChannelMpesaC2b, Receipt: r, Amount: money.KES(c.Amount), Phone: "254712345678",
			AccountNumber: c.AccountNumber, AppID: &c.AppID, ChargeID: &c.ID, RawPayload: []byte(`{"TransID":"` + r + `"}`),
		})
		if err != nil {
			return err
		}
		_, err = q.MarkChargeSucceeded(ctx, store.MarkChargeSucceededParams{TransactionID: &txn.ID, ID: c.ID})
		return err
	})
	if err != nil {
		t.Fatalf("seed pay charge: %v", err)
	}
	return txn
}
