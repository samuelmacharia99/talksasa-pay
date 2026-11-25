package adminapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// LedgerService is what the admin API needs from the ledger
// (*ledger.Service implements it; tests use a fake). Both calls audit the
// action with the staff member as actor and emit the app's event
// (payment.received, transaction.reversed) in the same database transaction.
type LedgerService interface {
	// Reverse records a reversal of a succeeded incoming payment (money
	// rule 9).
	Reverse(ctx context.Context, actor audit.Actor, p ledger.ReverseParams) (ledger.ReverseResult, error)
	// AssignToApp routes an unmatched payment (app_id NULL) to an app.
	AssignToApp(ctx context.Context, actor audit.Actor, txnID, appID uuid.UUID, reason string) (ledger.Transaction, error)
}

// appGetter looks up an app (*app.Service implements it; tests use a fake).
type appGetter interface {
	Get(ctx context.Context, id uuid.UUID) (app.App, error)
}

func (h *handlers) appsGetter() appGetter {
	if h.ap != nil {
		return h.ap
	}
	return h.d.Apps
}

const (
	minReasonLen = 3
	maxReasonLen = 500
)

func conflict(w http.ResponseWriter, r *http.Request, code, detail string) {
	httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusConflict, code, detail))
}

func ledgerUnavailable(w http.ResponseWriter, r *http.Request) {
	httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusServiceUnavailable, "ledger_unavailable", "Recording this correction is not available in this deployment."))
}

// validReason trims and length-checks a staff reason, writing 422 if bad.
func validReason(w http.ResponseWriter, r *http.Request, raw string) (string, bool) {
	reason := strings.TrimSpace(raw)
	if len(reason) < minReasonLen || len(reason) > maxReasonLen {
		validationFailed(w, r, "reason", "Give a reason of 3-500 characters.")
		return "", false
	}
	return reason, true
}

// assignable reports why a transaction cannot be assigned to an app ("" if
// it can): only succeeded incoming payments with no app (unmatched).
func assignable(t store.GetTransactionAdminRow) (code, detail string) {
	if t.AppID != nil {
		return "already_assigned", "This payment already belongs to an app."
	}
	if t.Direction != store.TxnDirectionIn || t.Kind != store.TxnKindPayment || t.Status != store.TxnStatusSucceeded {
		return "not_assignable", "Only succeeded incoming payments can be assigned to an app."
	}
	return "", ""
}

// assignTransaction: POST /transactions/{id}/assign {app_id, reason}.
// Staff (finance, admin) route an unmatched payment to an app; the app then
// gets payment.received. Set once, audited with the reason.
func (h *handlers) assignTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		AppID  string `json:"app_id"`
		Reason string `json:"reason"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	appID, err := uuid.Parse(strings.TrimSpace(in.AppID))
	if err != nil {
		validationFailed(w, r, "app_id", "Choose the app this payment belongs to.")
		return
	}
	reason, ok := validReason(w, r, in.Reason)
	if !ok {
		return
	}
	t, err := h.reads().GetTransaction(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if code, detail := assignable(t.Txn); code != "" {
		conflict(w, r, code, detail)
		return
	}
	// Assigning to a suspended app is allowed (money rule 8): the response
	// carries app_status so the panel can warn.
	if _, err := h.appsGetter().Get(r.Context(), appID); err != nil {
		if errors.Is(err, app.ErrNotFound) {
			validationFailed(w, r, "app_id", "No such app.")
			return
		}
		h.fail(w, r, err)
		return
	}
	if h.d.Ledger == nil {
		ledgerUnavailable(w, r)
		return
	}
	txn, err := h.d.Ledger.AssignToApp(r.Context(), principalFrom(r.Context()).Actor(), id, appID, reason)
	if err != nil {
		// The checks above ran without a lock: another staff member may
		// have assigned it in between. Re-read to answer precisely.
		if now, gerr := h.reads().GetTransaction(r.Context(), id); gerr == nil {
			if code, detail := assignable(now.Txn); code != "" {
				conflict(w, r, code, detail)
				return
			}
		}
		h.fail(w, r, err)
		return
	}
	d, err := h.reads().GetTransaction(r.Context(), txn.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toTxnDetail(d))
}

// reverseTransaction: POST /transactions/{id}/reverse {reason, receipt?}.
// Admin only (money rule 9): records a reversal transaction; a charge the
// payment paid becomes reversed and the app gets transaction.reversed.
func (h *handlers) reverseTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Reason  string `json:"reason"`
		Receipt string `json:"receipt"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	reason, ok := validReason(w, r, in.Reason)
	if !ok {
		return
	}
	receipt := strings.ToUpper(strings.TrimSpace(in.Receipt))
	if receipt != "" && !ledger.ValidReceipt(receipt) {
		validationFailed(w, r, "receipt", "Receipt must be 1-64 letters or digits.")
		return
	}
	t, err := h.reads().GetTransaction(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	o := t.Txn
	if o.Direction != store.TxnDirectionIn || o.Kind != store.TxnKindPayment || o.Status != store.TxnStatusSucceeded || o.AppID == nil {
		conflict(w, r, "not_reversible", "Only succeeded incoming payments that belong to an app can be reversed. Assign an unmatched payment first.")
		return
	}
	if o.Provider == "mpesa" && receipt == "" {
		validationFailed(w, r, "receipt", "Enter Safaricom's reversal receipt for an M-Pesa payment.")
		return
	}
	if h.d.Ledger == nil {
		ledgerUnavailable(w, r)
		return
	}
	res, err := h.d.Ledger.Reverse(r.Context(), principalFrom(r.Context()).Actor(), ledger.ReverseParams{
		OriginalID: id, Reason: reason, ProviderReceipt: receipt,
	})
	switch {
	case err == nil:
	case errors.Is(err, ledger.ErrAlreadyReversed):
		conflict(w, r, "already_reversed", "This payment has already been reversed.")
		return
	case errors.Is(err, ledger.ErrNotReversible):
		conflict(w, r, "not_reversible", "Only succeeded incoming payments that belong to an app can be reversed.")
		return
	case errors.Is(err, ledger.ErrReceiptConflict), db.IsUniqueViolation(err, "transactions_provider_receipt_uq"):
		conflict(w, r, "receipt_taken", "This receipt is already recorded on another transaction.")
		return
	case errors.Is(err, ledger.ErrReversalReceipt), errors.Is(err, ledger.ErrMissingReceipt):
		validationFailed(w, r, "receipt", "Enter Safaricom's reversal receipt for an M-Pesa payment.")
		return
	case errors.Is(err, ledger.ErrInvalidReceipt):
		validationFailed(w, r, "receipt", "Receipt must be 1-64 letters or digits.")
		return
	case errors.Is(err, ledger.ErrReasonRequired):
		validationFailed(w, r, "reason", "Give a reason of 3-500 characters.")
		return
	default:
		h.fail(w, r, err)
		return
	}
	d, err := h.reads().GetTransaction(r.Context(), res.Reversal.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toTxnDetail(d))
}

var _ reads = (*reporting.Service)(nil)
