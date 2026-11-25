package appapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/stk"
)

// STKService is the part of *stk.Service the app API uses (an interface
// so handler tests can use a fake). Leave Deps.STK as a nil interface (not a
// typed nil *stk.Service) when M-Pesa is not configured.
type STKService interface {
	Available() bool
	Send(ctx context.Context, actor audit.Actor, p stk.SendParams) (stk.Push, bool, error)
	Get(ctx context.Context, appID, id uuid.UUID) (stk.Push, error)
}

var _ STKService = (*stk.Service)(nil)

type stkPushInput struct {
	Phone *string `json:"phone"`
}

const maxPhoneInput = 20

func mpesaUnavailable(w http.ResponseWriter, r *http.Request) {
	problem(w, r, http.StatusServiceUnavailable, "mpesa_unavailable", "M-Pesa payment prompts are not available.")
}

// sendSTKPush: POST /charges/{id}/stk-push. The amount is always the
// charge's exact amount (decided by the service); the app id comes only
// from the API key.
func (h *handlers) sendSTKPush(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	chargeID, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var in stkPushInput
	if !decode(w, r, &in) {
		return
	}
	switch {
	case in.Phone == nil:
		invalid(w, r, http.StatusUnprocessableEntity, fieldErr("phone", "is required"))
		return
	case len(*in.Phone) > maxPhoneInput:
		invalid(w, r, http.StatusUnprocessableEntity, fieldErr("phone", phoneMessage))
		return
	}
	if _, err := phone.Normalize(*in.Phone); err != nil {
		invalid(w, r, http.StatusUnprocessableEntity, fieldErr("phone", phoneMessage))
		return
	}
	if h.d.STK == nil || !h.d.STK.Available() {
		mpesaUnavailable(w, r)
		return
	}
	appID := principalFrom(r.Context()).AppID
	push, replayed, err := h.d.STK.Send(r.Context(), audit.App(appID), stk.SendParams{
		AppID: appID, ChargeID: chargeID, Phone: *in.Phone, Source: stk.SourceApp, IdempotencyKey: key,
	})
	if err != nil {
		h.failSTK(w, r, push, err)
		return
	}
	status := http.StatusAccepted
	if replayed {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replayed", "true")
	}
	httpx.WriteJSON(w, status, apiview.STKPushOf(push))
}

const phoneMessage = "must be a Kenyan Safaricom number, e.g. 07XXXXXXXX, 01XXXXXXXX or +2547XXXXXXXX"

// failSTK maps stk.Service errors to the problems documented in
// api/app.openapi.yaml. push is what Send returned with err (set for
// ErrDaraja).
func (h *handlers) failSTK(w http.ResponseWriter, r *http.Request, push stk.Push, err error) {
	var inProgress *stk.InProgressError
	switch {
	case errors.Is(err, stk.ErrUnavailable):
		mpesaUnavailable(w, r)
	case errors.Is(err, stk.ErrNotFound):
		problem(w, r, http.StatusNotFound, "not_found", "No such resource.")
	case errors.Is(err, stk.ErrNotPayable):
		problem(w, r, http.StatusConflict, "not_payable", "This charge is not payable (succeeded, in review, cancelled or expired).")
	case errors.Is(err, stk.ErrAmountTooLarge):
		problem(w, r, http.StatusConflict, "amount_too_large",
			"The charge amount is above the M-Pesa limit of KES 250,000. Ask the customer to pay by paybill.")
	case errors.As(err, &inProgress):
		problem(w, r, http.StatusConflict, "stk_in_progress",
			"A payment prompt for this charge is already in progress: stk push "+inProgress.LiveID.String()+
				". Wait for its outcome before sending another.")
	case errors.Is(err, stk.ErrIdempotencyConflict):
		problem(w, r, http.StatusConflict, "idempotency_conflict",
			"This Idempotency-Key was already used with a different phone or charge. Use a new key for a new prompt.")
	case errors.Is(err, stk.ErrInvalidIdempotency):
		invalid(w, r, http.StatusBadRequest, fieldErr("Idempotency-Key", "must be 1-255 printable characters"))
	case errors.Is(err, phone.ErrInvalid):
		invalid(w, r, http.StatusUnprocessableEntity, fieldErr("phone", phoneMessage))
	case errors.Is(err, stk.ErrDaraja):
		detail := "Safaricom did not accept the payment prompt; nothing was charged. Ask the customer to pay by paybill or try again later."
		if push.ID != uuid.Nil {
			detail = "Safaricom did not accept the payment prompt (stk push " + push.ID.String() +
				" is failed); nothing was charged. Ask the customer to pay by paybill or try again later."
		}
		problem(w, r, http.StatusBadGateway, "mpesa_error", detail)
	case db.IsBusy(err):
		httpx.Busy(w, r, time.Second)
	default:
		h.fail(w, r, err)
	}
}

// getSTKPush: GET /stk-pushes/{id}, scoped to the key's app.
func (h *handlers) getSTKPush(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	if h.d.STK == nil {
		problem(w, r, http.StatusNotFound, "not_found", "No such resource.")
		return
	}
	push, err := h.d.STK.Get(r.Context(), principalFrom(r.Context()).AppID, id)
	// The service scopes by app; the check here is defence in depth.
	if errors.Is(err, stk.ErrNotFound) || db.IsNotFound(err) || (err == nil && push.AppID != principalFrom(r.Context()).AppID) {
		problem(w, r, http.StatusNotFound, "not_found", "No such resource.")
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, apiview.STKPushOf(push))
}
