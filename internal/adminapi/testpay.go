package adminapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/testpay"
)

// TestPayments runs real test payments from the Settings page
// (*testpay.Service implements it). nil: the routes answer 503.
type TestPayments interface {
	Start(ctx context.Context, actor audit.Actor, p testpay.StartParams) (testpay.Test, error)
	Recent(ctx context.Context, limit int) ([]testpay.Test, error)
}

type testPaymentJSON struct {
	ChargeID      uuid.UUID    `json:"charge_id"`
	AppID         uuid.UUID    `json:"app_id"`
	Reference     string       `json:"reference"`
	Amount        int64        `json:"amount"`
	AccountNumber string       `json:"account_number"`
	ChargeStatus  string       `json:"charge_status"`
	TransactionID *uuid.UUID   `json:"transaction_id"`
	SucceededAt   *time.Time   `json:"succeeded_at"`
	CreatedAt     time.Time    `json:"created_at"`
	Push          *stkPushJSON `json:"push"`
	// PromptError: why the prompt could not be sent (start only).
	PromptError *string `json:"prompt_error"`
}

func toTestPayment(t testpay.Test) testPaymentJSON {
	c := t.Charge
	out := testPaymentJSON{
		ChargeID: c.ID, AppID: c.AppID, Reference: c.Reference, Amount: c.Amount, AccountNumber: c.AccountNumber,
		ChargeStatus: string(c.Status), TransactionID: c.TransactionID, SucceededAt: c.SucceededAt, CreatedAt: c.CreatedAt,
	}
	if t.Push != nil {
		p := toSTKPush(*t.Push)
		out.Push = &p
	}
	if t.PromptError != nil {
		msg := promptErrorMessage(t.PromptError)
		out.PromptError = &msg
	}
	return out
}

// promptErrorMessage explains, for staff, why the prompt was not sent.
func promptErrorMessage(err error) string {
	switch {
	case errors.Is(err, testpay.ErrUnavailable):
		return "M-Pesa prompts are not available with the settings in effect."
	default:
		// stk errors are written for people (Safaricom's own text for a refusal).
		return strings.TrimPrefix(err.Error(), "stk: ")
	}
}

func testPaymentsUnavailable(w http.ResponseWriter, r *http.Request) {
	httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusServiceUnavailable, "settings_unavailable", "Test payments are not available in this deployment."))
}

func (h *handlers) failTestPayment(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, testpay.ErrInvalidAmount):
		validationFailed(w, r, "amount", fmt.Sprintf("Enter a whole number of shillings from %d to %d.", testpay.MinAmount, testpay.MaxAmount))
	case errors.Is(err, testpay.ErrInvalidPhone):
		validationFailed(w, r, "phone", "Enter a Safaricom number, e.g. 0712 345 678.")
	case errors.Is(err, testpay.ErrInvalidKey):
		validationFailed(w, r, "key", "This form was already used for a different test. Reload the page and try again.")
	case errors.Is(err, testpay.ErrConfirm):
		validationFailed(w, r, "confirm", `This sends real money on the production paybill: type "production" to confirm.`)
	case errors.Is(err, testpay.ErrUnavailable):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusConflict, "mpesa_not_ready",
			"M-Pesa prompts can't be sent with the settings in effect. Save complete paybill settings and test the connection first."))
	case errors.Is(err, testpay.ErrAppNotActive):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusConflict, "test_app_suspended",
			"The Gateway test app is suspended. Reactivate it on the Apps page to run test payments."))
	case errors.Is(err, app.ErrGatewayTestPrefixTaken):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusConflict, "prefix_taken",
			"Another app uses the account prefix "+app.GatewayTestPrefix+", which test payments need."))
	default:
		h.failSettings(w, r, err)
	}
}

type startTestPaymentBody struct {
	Amount  int64   `json:"amount"`
	Phone   string  `json:"phone"`
	Key     string  `json:"key"`
	Confirm *string `json:"confirm"`
}

func (h *handlers) startTestPayment(w http.ResponseWriter, r *http.Request) {
	if h.d.TestPayments == nil {
		testPaymentsUnavailable(w, r)
		return
	}
	var b startTestPaymentBody
	if !h.decode(w, r, &b) {
		return
	}
	p := testpay.StartParams{Amount: b.Amount, Phone: b.Phone, Key: b.Key}
	if b.Confirm != nil {
		p.Confirm = *b.Confirm
	}
	t, err := h.d.TestPayments.Start(r.Context(), principalFrom(r.Context()).Actor(), p)
	if err != nil {
		h.failTestPayment(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, toTestPayment(t))
}

func (h *handlers) listTestPayments(w http.ResponseWriter, r *http.Request) {
	if h.d.TestPayments == nil {
		testPaymentsUnavailable(w, r)
		return
	}
	tests, err := h.d.TestPayments.Recent(r.Context(), testpay.RecentLimit)
	if err != nil {
		h.failTestPayment(w, r, err)
		return
	}
	items := make([]testPaymentJSON, 0, len(tests))
	for _, t := range tests {
		items = append(items, toTestPayment(t))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}
