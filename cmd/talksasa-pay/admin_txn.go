package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/adminapi"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// adminLedger returns the ledger as the admin API and CLI use it, or nil
// when this build's ledger cannot assign payments yet.
//
// TODO(gateway-core): once ledger.Service.AssignToApp lands this is just
// `return l` (and can be inlined).
func adminLedger(l *ledger.Service) adminapi.LedgerService {
	if s, ok := any(l).(adminapi.LedgerService); ok {
		return s
	}
	return nil
}

var errNoLedger = errors.New("this build cannot assign payments yet (ledger.AssignToApp is missing)")

// txnAssign routes an unmatched payment (unknown account prefix) to an app.
// The app then gets payment.received. Audited with the operator and reason.
func txnAssign(ctx context.Context, a *services, actor audit.Actor, txnRef, appRef, reason string, w io.Writer) error {
	id, err := parseTxnID(txnRef)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(reason)) < 3 {
		return usageError{"--reason (at least 3 characters) is required"}
	}
	t, err := loadTxn(ctx, a, id)
	if err != nil {
		return err
	}
	if t.AppID != nil {
		return fmt.Errorf("transaction %s already belongs to an app (%s)", id, t.AppID)
	}
	if t.Direction != store.TxnDirectionIn || t.Kind != store.TxnKindPayment || t.Status != store.TxnStatusSucceeded {
		return fmt.Errorf("transaction %s is not a succeeded incoming payment (%s %s %s)", id, t.Direction, t.Kind, t.Status)
	}
	ap, err := a.apps.Resolve(ctx, appRef)
	if err != nil {
		return fmt.Errorf("--app %q: %w", appRef, err)
	}
	l := adminLedger(a.ledger)
	if l == nil {
		return errNoLedger
	}
	txn, err := l.AssignToApp(ctx, actor, id, ap.ID, strings.TrimSpace(reason))
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Payment %s (KES %d, receipt %s) assigned to %s (%s)\n", txn.ID, txn.Amount, deref(txn.ProviderTransactionID), ap.Name, ap.AccountPrefix)
	if ap.Status != store.AppStatusActive {
		fmt.Fprintf(w, "WARNING: %s is %s; the payment was still forwarded (money rule 8).\n", ap.AccountPrefix, ap.Status)
	}
	return nil
}

// txnReverse records a reversal of a succeeded incoming payment (money rule
// 9). The charge it paid becomes reversed and the app gets
// transaction.reversed. Audited with the operator and reason.
func txnReverse(ctx context.Context, a *services, actor audit.Actor, txnRef, reason, receipt string, w io.Writer) error {
	id, err := parseTxnID(txnRef)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(reason)) < 3 {
		return usageError{"--reason (at least 3 characters) is required"}
	}
	t, err := loadTxn(ctx, a, id)
	if err != nil {
		return err
	}
	if t.Direction != store.TxnDirectionIn || t.Kind != store.TxnKindPayment || t.Status != store.TxnStatusSucceeded || t.AppID == nil {
		return fmt.Errorf("transaction %s cannot be reversed: only succeeded incoming payments that belong to an app can be", id)
	}
	receipt = strings.ToUpper(strings.TrimSpace(receipt))
	if t.Provider == "mpesa" && receipt == "" {
		return usageError{"--receipt (Safaricom's reversal receipt) is required for an M-Pesa payment"}
	}
	res, err := a.ledger.Reverse(ctx, actor, ledger.ReverseParams{OriginalID: id, Reason: strings.TrimSpace(reason), ProviderReceipt: receipt})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Payment %s (KES %d) reversed by transaction %s\n", id, t.Amount, res.Reversal.ID)
	if res.Charge != nil {
		fmt.Fprintf(w, "Charge %s (%s) is now %s\n", res.Charge.ID, res.Charge.Reference, res.Charge.Status)
	}
	return nil
}

func parseTxnID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(s))
	if err != nil {
		return uuid.UUID{}, usageError{"--txn must be a transaction id (UUID)"}
	}
	return id, nil
}

func loadTxn(ctx context.Context, a *services, id uuid.UUID) (store.Transaction, error) {
	t, err := store.New(a.pool).GetTransactionInternal(ctx, id)
	if db.IsNotFound(err) {
		return t, fmt.Errorf("no transaction %s", id)
	}
	return t, err
}

func deref(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}
