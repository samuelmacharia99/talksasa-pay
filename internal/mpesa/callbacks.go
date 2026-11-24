package mpesa

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Callback parse errors. They are recorded on the inbox row and raise an
// alarm; the raw body is always kept.
var (
	ErrMalformed  = errors.New("mpesa: malformed callback")
	ErrBadAmount  = errors.New("mpesa: callback amount is not a whole, positive number of shillings")
	ErrNoReceipt  = errors.New("mpesa: successful STK callback has no receipt number")
	ErrBadReceipt = errors.New("mpesa: receipt number is not 1-64 letters or digits")
)

// MaxAmount is the largest single M-Pesa transaction we accept, in KES.
const MaxAmount = 250000

// STKResult is a parsed STK push callback.
type STKResult struct {
	MerchantRequestID string
	CheckoutRequestID string
	ResultCode        string // "0" is success
	ResultDesc        string
	// Set only on success.
	Receipt    string // M-Pesa receipt, upper-case
	Amount     int64
	Phone      string // as sent (2547...), may be empty
	OccurredAt *time.Time
}

// Succeeded reports whether the customer paid.
func (r STKResult) Succeeded() bool { return r.ResultCode == ResultSuccess }

type stkEnvelope struct {
	Body struct {
		STKCallback *struct {
			MerchantRequestID string      `json:"MerchantRequestID"`
			CheckoutRequestID string      `json:"CheckoutRequestID"`
			ResultCode        json.Number `json:"ResultCode"`
			ResultDesc        string      `json:"ResultDesc"`
			CallbackMetadata  *struct {
				Item []struct {
					Name  string `json:"Name"`
					Value any    `json:"Value"`
				} `json:"Item"`
			} `json:"CallbackMetadata"`
		} `json:"stkCallback"`
	} `json:"Body"`
}

// ParseSTKCallback parses Daraja's STK callback body. A successful result
// must carry a receipt and a whole-shilling amount, or an error is returned
// (the caller records it for staff; it never guesses).
func ParseSTKCallback(raw []byte) (STKResult, error) {
	var env stkEnvelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&env); err != nil || env.Body.STKCallback == nil {
		return STKResult{}, ErrMalformed
	}
	cb := env.Body.STKCallback
	r := STKResult{
		MerchantRequestID: strings.TrimSpace(cb.MerchantRequestID),
		CheckoutRequestID: strings.TrimSpace(cb.CheckoutRequestID),
		ResultCode:        strings.TrimSpace(cb.ResultCode.String()),
		ResultDesc:        truncate(strings.TrimSpace(cb.ResultDesc), 500),
	}
	if r.CheckoutRequestID == "" || len(r.CheckoutRequestID) > 128 || r.ResultCode == "" || len(r.ResultCode) > 32 {
		return STKResult{}, ErrMalformed
	}
	if !r.Succeeded() {
		return r, nil
	}
	if cb.CallbackMetadata == nil {
		return r, ErrNoReceipt
	}
	var haveAmount bool
	for _, it := range cb.CallbackMetadata.Item {
		v := scalar(it.Value)
		switch it.Name {
		case "Amount":
			n, err := parseAmount(v)
			if err != nil {
				return r, err
			}
			r.Amount, haveAmount = n, true
		case "MpesaReceiptNumber":
			r.Receipt = strings.ToUpper(strings.TrimSpace(v))
		case "PhoneNumber":
			r.Phone = strings.TrimSpace(v)
		case "TransactionDate":
			if t, ok := parseTime(v); ok {
				r.OccurredAt = &t
			}
		}
	}
	if r.Receipt == "" {
		return r, ErrNoReceipt
	}
	if !receiptRe.MatchString(r.Receipt) {
		return r, ErrBadReceipt
	}
	if !haveAmount {
		return r, ErrBadAmount
	}
	return r, nil
}

// C2BConfirmation is a parsed C2B (paybill) confirmation.
type C2BConfirmation struct {
	Receipt    string // TransID, upper-case
	Amount     int64
	BillRef    string // account number as the customer typed it
	ShortCode  string
	Phone      string // MSISDN if it is a usable number; Safaricom now masks it, then ""
	OccurredAt *time.Time
	TransType  string
}

type c2bBody struct {
	TransactionType   string `json:"TransactionType"`
	TransID           string `json:"TransID"`
	TransTime         any    `json:"TransTime"`
	TransAmount       any    `json:"TransAmount"`
	BusinessShortCode any    `json:"BusinessShortCode"`
	BillRefNumber     string `json:"BillRefNumber"`
	MSISDN            any    `json:"MSISDN"`
}

// ParseC2BConfirmation parses a C2B confirmation body.
func ParseC2BConfirmation(raw []byte) (C2BConfirmation, error) {
	var b c2bBody
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&b); err != nil {
		return C2BConfirmation{}, ErrMalformed
	}
	c := C2BConfirmation{
		Receipt:   strings.ToUpper(strings.TrimSpace(b.TransID)),
		BillRef:   truncate(strings.TrimSpace(b.BillRefNumber), 64),
		ShortCode: strings.TrimSpace(scalar(b.BusinessShortCode)),
		TransType: truncate(strings.TrimSpace(b.TransactionType), 64),
	}
	if c.Receipt == "" {
		return c, ErrNoReceipt
	}
	if !receiptRe.MatchString(c.Receipt) {
		return c, ErrBadReceipt
	}
	n, err := parseAmount(scalar(b.TransAmount))
	if err != nil {
		return c, err
	}
	c.Amount = n
	if t, ok := parseTime(scalar(b.TransTime)); ok {
		c.OccurredAt = &t
	}
	if p := strings.TrimSpace(scalar(b.MSISDN)); phoneDigitsRe.MatchString(p) {
		c.Phone = p
	}
	return c, nil
}

var (
	receiptRe     = regexp.MustCompile(`^[A-Z0-9]{1,64}$`)
	amountRe      = regexp.MustCompile(`^[0-9]{1,9}(\.0*)?$`)
	phoneDigitsRe = regexp.MustCompile(`^(254|0)?[17][0-9]{8}$`)
	timeRe        = regexp.MustCompile(`^[0-9]{14}$`)
)

// parseAmount accepts "100", "100.00", 100, 100.0; rejects fractions,
// negatives, zero and anything above MaxAmount.
func parseAmount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if !amountRe.MatchString(s) {
		return 0, ErrBadAmount
	}
	whole, _, _ := strings.Cut(s, ".")
	n, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || n <= 0 || n > MaxAmount {
		return 0, ErrBadAmount
	}
	return n, nil
}

// parseTime parses Daraja's yyyyMMddHHmmss (Nairobi time).
func parseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if !timeRe.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("20060102150405", s, Nairobi)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// scalar renders a decoded JSON scalar (string or json.Number) as text.
func scalar(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && !utf8RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Timestamp is Daraja's request timestamp (Nairobi time).
func Timestamp(t time.Time) string { return t.In(Nairobi).Format("20060102150405") }
