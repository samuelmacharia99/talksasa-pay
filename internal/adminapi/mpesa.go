package adminapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// STKService is what the admin API needs from the STK service
// (*stk.Service implements it; tests use a fake).
type STKService interface {
	Resolve(ctx context.Context, actor audit.Actor, p stk.ResolveParams) (store.Transaction, error)
	ListForCharge(ctx context.Context, chargeID uuid.UUID, limit int) ([]stk.Push, error)
}

// reads are the reporting queries behind the M-Pesa screens
// (*reporting.Service implements it; tests use a fake).
type reads interface {
	GetTransaction(ctx context.Context, id uuid.UUID) (reporting.TxnDetail, error)
	GetCharge(ctx context.Context, id uuid.UUID) (reporting.ChargeDetail, error)
	ListMpesaCallbacks(ctx context.Context, f reporting.CallbackFilter) (reporting.Page[store.ListMpesaCallbacksAttentionRow], error)
}

// stkListLimit bounds the pushes shown on a charge page.
const stkListLimit = 100

type stkPushJSON struct {
	ID                uuid.UUID `json:"id"`
	Source            string    `json:"source"`
	Status            string    `json:"status"`
	Amount            int64     `json:"amount"`
	Phone             string    `json:"phone"` // masked
	CheckoutRequestID *string   `json:"checkout_request_id"`
	ResultCode        *string   `json:"result_code"`
	ResultDesc        *string   `json:"result_desc"`
	QueryAttempts     int32     `json:"query_attempts"`
	TransactionID     uuid.UUID `json:"transaction_id"`
	// PaidTransactionID: the payment arrived after the push was given up
	// and was recorded on this new transaction (transaction_id is failed).
	PaidTransactionID *uuid.UUID `json:"paid_transaction_id"`
	// ConfirmBlocked: why staff cannot confirm it with a receipt.
	ConfirmBlocked *string    `json:"confirm_blocked"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at"`
}

func toSTKPush(p stk.Push) stkPushJSON {
	return stkPushJSON{ID: p.ID, Source: p.Source, Status: string(p.Status), Amount: p.Amount, Phone: tlog.MaskPhone(p.Phone),
		CheckoutRequestID: p.CheckoutRequestID, ResultCode: p.ResultCode, ResultDesc: p.ResultDesc,
		QueryAttempts: p.QueryAttempts, TransactionID: p.TransactionID,
		PaidTransactionID: p.PaidTransactionID, ConfirmBlocked: p.ConfirmBlocked, CreatedAt: p.CreatedAt, CompletedAt: p.CompletedAt}
}

type mpesaCallbackJSON struct {
	ID            uuid.UUID  `json:"id"`
	Kind          string     `json:"kind"`
	RemoteIP      *string    `json:"remote_ip"`
	ReceivedAt    time.Time  `json:"received_at"`
	ProcessedAt   *time.Time `json:"processed_at"`
	Error         *string    `json:"error"`
	Attempts      int32      `json:"attempts"`
	TransactionID *uuid.UUID `json:"transaction_id"`
}

func toMpesaCallback(c store.ListMpesaCallbacksAttentionRow) mpesaCallbackJSON {
	out := mpesaCallbackJSON{ID: c.ID, Kind: string(c.Kind), ReceivedAt: c.ReceivedAt, ProcessedAt: c.ProcessedAt,
		Error: c.Error, Attempts: c.Attempts, TransactionID: c.TransactionID}
	if c.RemoteIp != nil {
		s := c.RemoteIp.String()
		out.RemoteIP = &s
	}
	return out
}

func validationFailed(w http.ResponseWriter, r *http.Request, field, msg string) {
	p := httpx.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "The request is not valid; see errors.")
	p.Errors = []httpx.FieldError{{Field: field, Message: msg}}
	httpx.WriteProblem(w, r, p)
}

func mpesaUnavailable(w http.ResponseWriter, r *http.Request) {
	httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusServiceUnavailable, "mpesa_unavailable", "M-Pesa is not configured in this deployment."))
}

// resolveTransaction confirms (with the receipt from the M-Pesa portal) or
// fails an M-Pesa transaction in needs_review. finance or admin; audited by
// the STK service with the staff member as actor.
func (h *handlers) resolveTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Action  string `json:"action"`
		Receipt string `json:"receipt"`
		Reason  string `json:"reason"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	p := stk.ResolveParams{TransactionID: id, Reason: strings.TrimSpace(in.Reason)}
	switch in.Action {
	case "confirm":
		p.Confirm = true
		p.Receipt = strings.ToUpper(strings.TrimSpace(in.Receipt))
		if p.Receipt == "" {
			validationFailed(w, r, "receipt", "The M-Pesa receipt is required to confirm a payment.")
			return
		}
	case "fail":
		if strings.TrimSpace(in.Receipt) != "" {
			validationFailed(w, r, "receipt", "Leave the receipt empty when marking a payment failed.")
			return
		}
	default:
		validationFailed(w, r, "action", "Action must be confirm or fail.")
		return
	}
	if len(in.Reason) > 500 {
		validationFailed(w, r, "reason", "Reason must be at most 500 characters.")
		return
	}
	if h.d.STK == nil {
		mpesaUnavailable(w, r)
		return
	}
	txn, err := h.d.STK.Resolve(r.Context(), principalFrom(r.Context()).Actor(), p)
	switch {
	case err == nil:
	case errors.Is(err, stk.ErrNotFound):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusNotFound, "not_found", "No such resource."))
		return
	case errors.Is(err, stk.ErrNotResolvable):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusConflict, "not_resolvable", "Only M-Pesa payments waiting for review can be resolved."))
		return
	case errors.Is(err, stk.ErrReceiptTaken):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusConflict, "receipt_taken", "This receipt is already recorded on another transaction."))
		return
	case errors.Is(err, stk.ErrInvalidReceipt):
		validationFailed(w, r, "receipt", "Receipt must be 1-64 letters or digits.")
		return
	case errors.Is(err, stk.ErrReasonRequired):
		validationFailed(w, r, "reason", "Give a reason of at least 3 characters.")
		return
	default:
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

func (h *handlers) listSTKPushes(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if _, err := h.reads().GetCharge(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	out := []stkPushJSON{}
	if h.d.STK != nil {
		pushes, err := h.d.STK.ListForCharge(r.Context(), id, stkListLimit)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		for _, p := range pushes {
			out = append(out, toSTKPush(p))
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handlers) listMpesaCallbacks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	attention := false
	switch q.Get("unprocessed") {
	case "", "false":
	case "true":
		attention = true
	default:
		h.fail(w, r, filterError("unprocessed must be true or false"))
		return
	}
	page, err := h.reads().ListMpesaCallbacks(r.Context(), reporting.CallbackFilter{
		AttentionOnly: attention, Cursor: q.Get("cursor"), Limit: queryLimit(q),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, mapPage(page, toMpesaCallback))
}
