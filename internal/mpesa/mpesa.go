// Package mpesa talks to Safaricom Daraja: STK push (Lipa na M-Pesa Online),
// STK push status queries, C2B URL registration, and parsing the callbacks
// Daraja sends back.
//
// Rules that matter for money:
//   - A payment is only ever recorded from a callback carrying an M-Pesa
//     receipt. An STK query that reports success has no receipt; callers turn
//     that into staff review, never into a paid request.
//   - Amounts are whole Kenya shillings. A callback amount with a fractional
//     part is rejected (ErrBadAmount) rather than rounded.
//   - Nothing here logs secrets, access tokens, the STK password or full
//     phone numbers.
package mpesa

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Client is the Daraja API used by the rest of the service. The real
// implementation is NewClient (client.go); tests use mpesatest.
type Client interface {
	// STKPush asks Safaricom to show a payment prompt on the customer's
	// phone. A nil error means Daraja ACCEPTED the request (ResponseCode 0);
	// the outcome arrives later by callback (or STKQuery).
	STKPush(ctx context.Context, req STKPushRequest) (STKPushResponse, error)
	// STKQuery asks for the outcome of a push.
	STKQuery(ctx context.Context, checkoutRequestID string) (STKQueryResult, error)
	// RegisterC2BURLs registers the confirmation (and validation) URLs for
	// the paybill. Talksasa does not use external validation.
	RegisterC2BURLs(ctx context.Context, req RegisterURLsRequest) error
}

// STKPushRequest is one prompt. CallBackURL is filled in by the client from
// configuration.
type STKPushRequest struct {
	Phone            string // normalised 2547XXXXXXXX / 2541XXXXXXXX
	Amount           int64  // whole KES, 1..250000
	AccountReference string // the charge's account number, e.g. HOST-1042 (max 12 chars per Daraja)
	TransactionDesc  string // short description (max 13 chars per Daraja)
}

// STKPushResponse is Daraja's synchronous acceptance.
type STKPushResponse struct {
	MerchantRequestID   string
	CheckoutRequestID   string
	ResponseCode        string
	ResponseDescription string
	CustomerMessage     string
}

// QueryState is what an STK query tells us.
type QueryState int

const (
	// QueryPending: Safaricom is still processing (or the customer has not
	// answered yet). Ask again later.
	QueryPending QueryState = iota
	// QuerySucceeded: Safaricom says the payment went through. NO receipt
	// is available from a query: callers must not record money from this.
	QuerySucceeded
	// QueryFailed: final failure (cancelled by user, timeout, insufficient
	// funds, wrong PIN, ...). ResultCode/ResultDesc say which.
	QueryFailed
)

func (s QueryState) String() string {
	switch s {
	case QueryPending:
		return "pending"
	case QuerySucceeded:
		return "succeeded"
	case QueryFailed:
		return "failed"
	}
	return fmt.Sprintf("QueryState(%d)", int(s))
}

// STKQueryResult is the outcome of an STK query.
type STKQueryResult struct {
	State      QueryState
	ResultCode string
	ResultDesc string
}

// RegisterURLsRequest registers C2B URLs for a shortcode.
type RegisterURLsRequest struct {
	ShortCode       string
	ConfirmationURL string
	ValidationURL   string
	// ResponseType is what Safaricom does if the validation URL cannot be
	// reached: "Completed" (accept the payment). We never reject payments.
	ResponseType string
}

// ErrorKind classifies a Daraja failure.
type ErrorKind int

const (
	// Transient: network error, timeout, 5xx, rate limiting, expired token.
	// The call may be retried; for STKPush the outcome is UNKNOWN (Daraja
	// may have sent the prompt), so callers must not assume it failed.
	Transient ErrorKind = iota + 1
	// Rejected: Daraja refused the request (bad phone, invalid amount,
	// wrong credentials). Retrying the same request will not help; for
	// STKPush no prompt was sent.
	Rejected
)

// Error is a typed Daraja failure. Message never contains secrets.
type Error struct {
	Kind       ErrorKind
	HTTPStatus int    // 0 when no response was received
	Code       string // Daraja errorCode / ResponseCode, if any
	Message    string // Daraja errorMessage / ResponseDescription, trimmed
	Err        error  // underlying network error, if any
}

func (e *Error) Error() string {
	kind := "transient"
	if e.Kind == Rejected {
		kind = "rejected"
	}
	s := "mpesa: " + kind
	if e.HTTPStatus != 0 {
		s += fmt.Sprintf(" (HTTP %d)", e.HTTPStatus)
	}
	if e.Code != "" {
		s += " " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// IsRejected reports whether err is a definite Daraja rejection.
func IsRejected(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == Rejected
}

// IsTransient reports whether err may succeed on retry (outcome unknown).
func IsTransient(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Kind == Transient
}

// ErrUnavailable means M-Pesa is not configured in this deployment.
var ErrUnavailable = errors.New("mpesa: not configured")

// Daraja STK result codes we name explicitly. Anything else non-zero is a
// final failure described by ResultDesc.
const (
	ResultSuccess             = "0"
	ResultInsufficientFunds   = "1"
	ResultCancelledByUser     = "1032"
	ResultUnreachable         = "1037" // DS timeout: phone could not be reached
	ResultWrongPIN            = "2001"
	ResultTransactionExpired  = "1019"
	ResultPushRequestInFlight = "1001" // another push is already being processed for this phone
)

// Nairobi is Safaricom's time zone (timestamps and STK passwords). Kenya has
// no daylight saving, so a fixed +03:00 zone is exact and needs no tzdata.
var Nairobi = time.FixedZone("EAT", 3*60*60)
