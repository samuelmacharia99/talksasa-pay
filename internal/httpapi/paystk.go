package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/productapi"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// STKService is the part of *stk.Service the public listener uses (an
// interface so handler tests can use a fake).
type STKService interface {
	productapi.STKService
	LatestForRequest(ctx context.Context, requestID uuid.UUID, since time.Time) (*stk.Push, error)
	StoreCallback(ctx context.Context, kind stk.CallbackKind, remoteIP netip.Addr, raw []byte) (uuid.UUID, error)
}

var _ STKService = (*stk.Service)(nil)

// payPageSTKWindow: the pay page shows the latest prompt this recent.
const payPageSTKWindow = 15 * time.Minute

// payPageActor is the audit actor for prompts sent from the pay page.
var payPageActor = audit.System("pay_page")

func stkAvailable(d Deps, p reporting.PayPage) bool {
	outstanding := p.Amount - p.AmountPaid
	return d.STK != nil && d.STK.Available() && p.Payable && outstanding > 0 && outstanding <= mpesa.MaxAmount
}

// latestSTK is the newest prompt for the bill in the last 15 minutes, or
// nil. A lookup failure is logged and shown as no prompt: the page itself
// (amount, paybill steps) must still load.
func latestSTK(d Deps, r *http.Request, p reporting.PayPage) *payPageSTK {
	if d.STK == nil {
		return nil
	}
	push, err := d.STK.LatestForRequest(r.Context(), p.RequestID, time.Now().Add(-payPageSTKWindow))
	if err != nil {
		d.Logger.ErrorContext(r.Context(), "pay page: latest stk push lookup failed", slog.String("error", err.Error()))
		return nil
	}
	if push == nil {
		return nil
	}
	v := payPageSTKOf(*push)
	return &v
}

// payPageSTKOf renders a push for the payer: status, a customer-facing
// reason when it failed, and when it was sent. No ids, no phone.
func payPageSTKOf(p stk.Push) payPageSTK {
	out := payPageSTK{Status: apiview.STKPushStatus(p.Status), CreatedAt: p.CreatedAt.UTC()}
	if p.Status == store.StkPushStatusFailed {
		code := ""
		if p.ResultCode != nil {
			code = *p.ResultCode
		}
		msg := customerMessage(code)
		out.Message = &msg
	}
	return out
}

// customerMessage explains a failed prompt to the payer in plain words.
// Result codes are Daraja's (STK callback / query ResultCode).
//
// TODO(integration): replace with stk.CustomerMessage once the STK service
// exports it, so the pay page and other surfaces share one wording.
func customerMessage(resultCode string) string {
	switch resultCode {
	case "1032":
		return "You cancelled the payment prompt."
	case "1037":
		return "The prompt timed out or your phone could not be reached. Make sure it is on and try again."
	case "1":
		return "Your M-Pesa balance is not enough for this payment."
	case "2001":
		return "The M-Pesa PIN entered was wrong. Please try again."
	case "1001":
		return "Another M-Pesa payment is in progress on your phone. Wait a moment and try again."
	case "1019":
		return "The payment prompt expired. Please try again."
	default:
		return "The payment did not go through. Please try again or pay by paybill."
	}
}

// maxPayPageSTKBody bounds the prompt request body ({"phone": "..."}).
const maxPayPageSTKBody = 1 << 10

// payPageSTKPush: POST /public/v1/pay/{token}/stk {phone}. Same 404 rules
// as GET /pay/{token}; per-IP rate limited (stricter than reads) by the
// router; the service allows one live prompt per bill. Responses never
// carry ids or the phone number.
func payPageSTKPush(d Deps) http.HandlerFunc {
	prob := func(w http.ResponseWriter, r *http.Request, status int, code, detail string) {
		httpx.WriteProblem(w, r, httpx.NewProblem(status, code, detail))
	}
	invalidPhone := func(w http.ResponseWriter, r *http.Request) {
		p := httpx.NewProblem(http.StatusUnprocessableEntity, "validation_failed", "Enter a Safaricom number, e.g. 07XXXXXXXX.")
		p.Errors = []httpx.FieldError{{Field: "phone", Message: "must be a Kenyan Safaricom number, e.g. 07XXXXXXXX or +2547XXXXXXXX"}}
		httpx.WriteProblem(w, r, p)
	}
	unavailable := func(w http.ResponseWriter, r *http.Request) {
		prob(w, r, http.StatusServiceUnavailable, "mpesa_unavailable", "M-Pesa payment prompts are not available right now. Pay by paybill instead.")
	}
	notPayable := func(w http.ResponseWriter, r *http.Request) {
		prob(w, r, http.StatusConflict, "not_payable", "This bill can no longer be paid.")
	}
	tooLarge := func(w http.ResponseWriter, r *http.Request) {
		prob(w, r, http.StatusConflict, "amount_too_large", "This balance is above the M-Pesa limit of KES 250,000. Pay by paybill instead.")
	}
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := lookupPayPage(d, w, r)
		if !ok {
			return
		}
		if d.STK == nil || !d.STK.Available() {
			unavailable(w, r)
			return
		}
		raw, ok := decodePhone(w, r)
		if !ok {
			return
		}
		if _, err := phone.Normalize(raw); err != nil {
			invalidPhone(w, r)
			return
		}
		// Cheap checks first; the service re-checks under a lock.
		if !p.Payable || p.Amount-p.AmountPaid <= 0 {
			notPayable(w, r)
			return
		}
		if p.Amount-p.AmountPaid > mpesa.MaxAmount {
			tooLarge(w, r)
			return
		}
		push, _, err := d.STK.Send(r.Context(), payPageActor, stk.SendParams{
			ProductID: p.ProductID, RequestID: p.RequestID, Phone: raw, Source: stk.SourcePayPage,
		})
		var inProgress *stk.InProgressError
		switch {
		case err == nil:
			httpx.WriteJSON(w, http.StatusAccepted, payPageSTKOf(push))
		case errors.Is(err, stk.ErrNotFound):
			payLinkNotFound(w, r)
		case errors.Is(err, stk.ErrUnavailable):
			unavailable(w, r)
		case errors.Is(err, stk.ErrNotPayable):
			notPayable(w, r)
		case errors.Is(err, stk.ErrAmountTooLarge):
			tooLarge(w, r)
		case errors.As(err, &inProgress):
			prob(w, r, http.StatusConflict, "stk_in_progress", "A payment prompt was already sent for this bill. Check your phone, or wait a minute and try again.")
		case errors.Is(err, phone.ErrInvalid):
			invalidPhone(w, r)
		case errors.Is(err, stk.ErrDaraja):
			prob(w, r, http.StatusBadGateway, "mpesa_error", "M-Pesa could not send the prompt; nothing was charged. Try again later or pay by paybill.")
		case db.IsBusy(err):
			httpx.Busy(w, r, time.Second)
		case errors.Is(err, context.DeadlineExceeded):
			w.Header().Set("Retry-After", "1")
			prob(w, r, http.StatusServiceUnavailable, "overloaded", "The service is busy. Retry shortly.")
		default:
			httpx.InternalError(w, r, d.Logger, err)
		}
	}
}

// decodePhone reads exactly {"phone": "<string>"} (no other fields). It
// writes the problem and returns false on any error.
func decodePhone(w http.ResponseWriter, r *http.Request) (string, bool) {
	fail := func(status int, code, detail string, errs ...httpx.FieldError) (string, bool) {
		p := httpx.NewProblem(status, code, detail)
		p.Errors = errs
		httpx.WriteProblem(w, r, p)
		return "", false
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		return fail(http.StatusUnsupportedMediaType, "unsupported_media_type", "Send Content-Type: application/json.")
	}
	if r.ContentLength > maxPayPageSTKBody {
		return fail(http.StatusRequestEntityTooLarge, "payload_too_large", "Request body is too large.")
	}
	var in struct {
		Phone *string `json:"phone"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPayPageSTKBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(&in)
	if err == nil {
		if _, terr := dec.Token(); terr != io.EOF {
			err = errors.New("trailing data")
		}
	}
	var tooBig *http.MaxBytesError
	var typeErr *json.UnmarshalTypeError
	switch {
	case err == nil && in.Phone == nil:
		return fail(http.StatusUnprocessableEntity, "validation_failed", "Enter a phone number.", httpx.FieldError{Field: "phone", Message: "is required"})
	case err == nil && len(*in.Phone) > 20:
		return fail(http.StatusUnprocessableEntity, "validation_failed", "Enter a Safaricom number, e.g. 07XXXXXXXX.",
			httpx.FieldError{Field: "phone", Message: "must be at most 20 characters"})
	case err == nil:
		return *in.Phone, true
	case errors.As(err, &tooBig):
		return fail(http.StatusRequestEntityTooLarge, "payload_too_large", "Request body is too large.")
	case errors.As(err, &typeErr) && typeErr.Field == "phone":
		return fail(http.StatusUnprocessableEntity, "validation_failed", "Enter a phone number.", httpx.FieldError{Field: "phone", Message: "must be a string"})
	default:
		// Unknown fields included: the body must be exactly {"phone": "..."}.
		return fail(http.StatusBadRequest, "invalid_json", `Send a JSON object {"phone": "07XXXXXXXX"}.`)
	}
}
