package mpesa

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/talksasa/talksasa-pay/internal/config"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
)

// Daraja base URLs.
const (
	SandboxBaseURL    = "https://sandbox.safaricom.co.ke/"
	ProductionBaseURL = "https://api.safaricom.co.ke/"
)

// Daraja API paths (relative to the base URL).
const (
	PathOAuth        = "oauth/v1/generate"
	PathSTKPush      = "mpesa/stkpush/v1/processrequest"
	PathSTKQuery     = "mpesa/stkpushquery/v1/query"
	PathRegisterV1   = "mpesa/c2b/v1/registerurl" // sandbox
	PathRegisterV2   = "mpesa/c2b/v2/registerurl" // production
	processingCode   = "500.001.1001"             // "The transaction is being processed"
	maxResponseBytes = 64 << 10
	maxMessageLen    = 200
	// Daraja field limits.
	maxAccountReference = 12
	maxTransactionDesc  = 13
)

// ResultProcessing is the STK query ResultCode some Daraja versions use for
// "still being processed".
const ResultProcessing = "4999"

// ClientOptions configures NewClient.
type ClientOptions struct {
	Env string // config.MpesaSandbox | config.MpesaProduction
	// BaseURL overrides the Daraja base URL (development fake). Empty picks
	// SandboxBaseURL or ProductionBaseURL from Env.
	BaseURL        string
	ShortCode      string
	ConsumerKey    config.Secret
	ConsumerSecret config.Secret
	Passkey        config.Secret
	CallbackToken  config.Secret
	// CallbackBaseURL is the public base Safaricom calls; ends in "/".
	CallbackBaseURL string
	// Timeout bounds one HTTP call (default 15s).
	Timeout    time.Duration
	Logger     *slog.Logger
	HTTPClient *http.Client     // optional (tests)
	Now        func() time.Time // optional
}

// HTTPClient is the real Daraja client. It is safe for concurrent use.
type HTTPClient struct {
	env       string
	base      string // ends in "/"
	shortCode string
	key       config.Secret
	secret    config.Secret
	passkey   config.Secret
	cbToken   config.Secret
	cbBase    string
	timeout   time.Duration
	hc        *http.Client
	log       *slog.Logger
	now       func() time.Time

	mu        sync.Mutex
	token     string
	refreshAt time.Time
	sf        singleflight.Group
}

var _ Client = (*HTTPClient)(nil)

var (
	shortCodeRe     = regexp.MustCompile(`^[0-9]{5,7}$`)
	callbackTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{32,}$`)
	stkPhoneRe      = regexp.MustCompile(`^254[17][0-9]{8}$`)
)

// errRedirect is returned for any 3xx: Daraja never redirects.
var errRedirect = errors.New("mpesa: redirects are not followed")

// NewClient validates o and builds a client. Errors never contain secrets.
func NewClient(o ClientOptions) (*HTTPClient, error) {
	var errs []error
	base := o.BaseURL
	switch o.Env {
	case config.MpesaSandbox:
		if base == "" {
			base = SandboxBaseURL
		}
	case config.MpesaProduction:
		if base == "" {
			base = ProductionBaseURL
		}
	default:
		errs = append(errs, fmt.Errorf("mpesa: Env must be %q or %q", config.MpesaSandbox, config.MpesaProduction))
	}
	if u, err := url.Parse(base); base != "" && (err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "") {
		errs = append(errs, errors.New("mpesa: BaseURL must be an absolute http(s) URL"))
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	if !shortCodeRe.MatchString(o.ShortCode) {
		errs = append(errs, errors.New("mpesa: ShortCode must be 5-7 digits"))
	}
	if o.ConsumerKey.IsZero() || o.ConsumerSecret.IsZero() || o.Passkey.IsZero() {
		errs = append(errs, errors.New("mpesa: ConsumerKey, ConsumerSecret and Passkey are required"))
	}
	if !callbackTokenRe.MatchString(o.CallbackToken.Reveal()) {
		errs = append(errs, errors.New("mpesa: CallbackToken must be at least 32 URL-safe characters"))
	}
	if u, err := url.Parse(o.CallbackBaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		!strings.HasSuffix(o.CallbackBaseURL, "/") || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, errors.New("mpesa: CallbackBaseURL must be an absolute http(s) URL ending in /"))
	}
	if o.Timeout < 0 {
		errs = append(errs, errors.New("mpesa: Timeout must not be negative"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if o.Timeout == 0 {
		o.Timeout = 15 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = newHTTPTransportClient(o.Timeout)
	}
	return &HTTPClient{
		env: o.Env, base: base, shortCode: o.ShortCode,
		key: o.ConsumerKey, secret: o.ConsumerSecret, passkey: o.Passkey, cbToken: o.CallbackToken,
		cbBase: o.CallbackBaseURL, timeout: o.Timeout, hc: hc, log: o.Logger, now: o.Now,
	}, nil
}

// newHTTPTransportClient: TLS 1.2+, no proxy from the environment (it would
// see the bearer token), no redirects, bounded timeouts.
func newHTTPTransportClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           20,
		MaxIdleConnsPerHost:    10,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    timeout,
		ResponseHeaderTimeout:  timeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: 16 << 10,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
}

// CallbackURLs returns the URLs Daraja is given (they contain the callback
// token: never log them).
func (c *HTTPClient) CallbackURLs() (stk, c2bConfirmation, c2bValidation string) {
	p := c.cbBase + "cb/mpesa/" + c.cbToken.Reveal()
	return p + "/stk", p + "/c2b/confirmation", p + "/c2b/validation"
}

// password is base64(shortcode + passkey + timestamp). Never log it.
func (c *HTTPClient) password(ts string) string {
	return base64.StdEncoding.EncodeToString([]byte(c.shortCode + c.passkey.Reveal() + ts))
}

// ---------------------------------------------------------------- STK push

type stkPushBody struct {
	BusinessShortCode string `json:"BusinessShortCode"`
	Password          string `json:"Password"`
	Timestamp         string `json:"Timestamp"`
	TransactionType   string `json:"TransactionType"`
	Amount            int64  `json:"Amount"`
	PartyA            string `json:"PartyA"`
	PartyB            string `json:"PartyB"`
	PhoneNumber       string `json:"PhoneNumber"`
	CallBackURL       string `json:"CallBackURL"`
	AccountReference  string `json:"AccountReference"`
	TransactionDesc   string `json:"TransactionDesc"`
}

type stkPushReply struct {
	MerchantRequestID   string `json:"MerchantRequestID"`
	CheckoutRequestID   string `json:"CheckoutRequestID"`
	ResponseCode        any    `json:"ResponseCode"`
	ResponseDescription string `json:"ResponseDescription"`
	CustomerMessage     string `json:"CustomerMessage"`
}

// STKPush sends a prompt. A *Error of kind Rejected means no prompt was sent;
// Transient means the outcome is unknown (query it, or wait for a callback).
func (c *HTTPClient) STKPush(ctx context.Context, req STKPushRequest) (STKPushResponse, error) {
	const op = "stk_push"
	if !stkPhoneRe.MatchString(req.Phone) {
		return STKPushResponse{}, &Error{Kind: Rejected, Code: "invalid_request", Message: "phone must be 2547XXXXXXXX or 2541XXXXXXXX"}
	}
	if req.Amount < 1 || req.Amount > MaxAmount {
		return STKPushResponse{}, &Error{Kind: Rejected, Code: "invalid_request", Message: fmt.Sprintf("amount must be 1-%d", MaxAmount)}
	}
	desc := strings.TrimSpace(req.TransactionDesc)
	if desc == "" {
		desc = "Payment"
	}
	ref := strings.TrimSpace(req.AccountReference)
	if ref == "" {
		ref = c.shortCode
	}
	stkURL, _, _ := c.CallbackURLs()
	ts := Timestamp(c.now())
	body := stkPushBody{
		BusinessShortCode: c.shortCode,
		Password:          c.password(ts),
		Timestamp:         ts,
		TransactionType:   "CustomerPayBillOnline",
		Amount:            req.Amount,
		PartyA:            req.Phone,
		PartyB:            c.shortCode,
		PhoneNumber:       req.Phone,
		CallBackURL:       stkURL,
		AccountReference:  truncate(ref, maxAccountReference),
		TransactionDesc:   truncate(desc, maxTransactionDesc),
	}
	res, ferr := c.call(ctx, op, PathSTKPush, body, req.Phone)
	if ferr != nil {
		// An error raised before the request reached Daraja (no token,
		// connection refused, DNS) means no prompt was sent.
		if ferr.notSent {
			ferr.Kind = Rejected
		}
		return STKPushResponse{}, ferr.Error
	}
	if res.status != http.StatusOK {
		return STKPushResponse{}, statusError(res)
	}
	var r stkPushReply
	if err := decodeJSON(res.body, &r); err != nil {
		// Accepted or not? We cannot tell: the outcome is unknown.
		return STKPushResponse{}, &Error{Kind: Transient, HTTPStatus: res.status, Message: "unreadable STK push response"}
	}
	out := STKPushResponse{
		MerchantRequestID:   strings.TrimSpace(r.MerchantRequestID),
		CheckoutRequestID:   strings.TrimSpace(r.CheckoutRequestID),
		ResponseCode:        strings.TrimSpace(scalar(r.ResponseCode)),
		ResponseDescription: clean(r.ResponseDescription),
		CustomerMessage:     clean(r.CustomerMessage),
	}
	if out.ResponseCode != ResultSuccess {
		if out.ResponseCode == "" {
			return STKPushResponse{}, &Error{Kind: Transient, HTTPStatus: res.status, Message: "STK push response has no ResponseCode"}
		}
		return STKPushResponse{}, &Error{Kind: Rejected, HTTPStatus: res.status, Code: clean(out.ResponseCode), Message: out.ResponseDescription}
	}
	if out.CheckoutRequestID == "" || len(out.CheckoutRequestID) > 128 {
		return STKPushResponse{}, &Error{Kind: Transient, HTTPStatus: res.status, Message: "STK push accepted without a usable CheckoutRequestID"}
	}
	return out, nil
}

// ---------------------------------------------------------------- STK query

type stkQueryBody struct {
	BusinessShortCode string `json:"BusinessShortCode"`
	Password          string `json:"Password"`
	Timestamp         string `json:"Timestamp"`
	CheckoutRequestID string `json:"CheckoutRequestID"`
}

type stkQueryReply struct {
	ResponseCode        any    `json:"ResponseCode"`
	ResponseDescription string `json:"ResponseDescription"`
	ResultCode          any    `json:"ResultCode"`
	ResultDesc          string `json:"ResultDesc"`
}

// STKQuery asks for a push's outcome. Errors are *Error; Transient ones may
// be retried.
func (c *HTTPClient) STKQuery(ctx context.Context, checkoutRequestID string) (STKQueryResult, error) {
	const op = "stk_query"
	id := strings.TrimSpace(checkoutRequestID)
	if id == "" || len(id) > 128 {
		return STKQueryResult{}, &Error{Kind: Rejected, Code: "invalid_request", Message: "CheckoutRequestID must be 1-128 characters"}
	}
	ts := Timestamp(c.now())
	res, ferr := c.call(ctx, op, PathSTKQuery, stkQueryBody{
		BusinessShortCode: c.shortCode, Password: c.password(ts), Timestamp: ts, CheckoutRequestID: id,
	}, "")
	if ferr != nil {
		if ferr.notSent && ferr.HTTPStatus == 0 {
			ferr.Kind = Transient // asking again is harmless
		}
		return STKQueryResult{}, ferr.Error
	}
	if res.status != http.StatusOK {
		if de := darajaError(res.body); de.code == processingCode {
			return STKQueryResult{State: QueryPending, ResultCode: processingCode, ResultDesc: de.message}, nil
		}
		return STKQueryResult{}, statusError(res)
	}
	var r stkQueryReply
	if err := decodeJSON(res.body, &r); err != nil {
		return STKQueryResult{}, &Error{Kind: Transient, HTTPStatus: res.status, Message: "unreadable STK query response"}
	}
	code := strings.TrimSpace(scalar(r.ResultCode))
	desc := clean(r.ResultDesc)
	switch code {
	case "":
		return STKQueryResult{}, &Error{Kind: Transient, HTTPStatus: res.status, Code: clean(scalar(r.ResponseCode)),
			Message: firstNonEmpty(clean(r.ResponseDescription), "STK query response has no ResultCode")}
	case ResultSuccess:
		return STKQueryResult{State: QuerySucceeded, ResultCode: code, ResultDesc: desc}, nil
	case ResultProcessing:
		return STKQueryResult{State: QueryPending, ResultCode: code, ResultDesc: desc}, nil
	default:
		return STKQueryResult{State: QueryFailed, ResultCode: clean(code), ResultDesc: desc}, nil
	}
}

// ---------------------------------------------------------------- C2B

type registerBody struct {
	ShortCode       string `json:"ShortCode"`
	ResponseType    string `json:"ResponseType"`
	ConfirmationURL string `json:"ConfirmationURL"`
	ValidationURL   string `json:"ValidationURL"`
}

type registerReply struct {
	ResponseCode        any    `json:"ResponseCode"`
	ResponseDescription string `json:"ResponseDescription"`
}

// RegisterC2BURLs registers the confirmation/validation URLs. Empty fields
// of req default to this client's shortcode, callback URLs and "Completed".
func (c *HTTPClient) RegisterC2BURLs(ctx context.Context, req RegisterURLsRequest) error {
	const op = "c2b_register"
	_, conf, val := c.CallbackURLs()
	body := registerBody{
		ShortCode:       firstNonEmpty(strings.TrimSpace(req.ShortCode), c.shortCode),
		ResponseType:    firstNonEmpty(strings.TrimSpace(req.ResponseType), "Completed"),
		ConfirmationURL: firstNonEmpty(strings.TrimSpace(req.ConfirmationURL), conf),
		ValidationURL:   firstNonEmpty(strings.TrimSpace(req.ValidationURL), val),
	}
	if !shortCodeRe.MatchString(body.ShortCode) {
		return &Error{Kind: Rejected, Code: "invalid_request", Message: "ShortCode must be 5-7 digits"}
	}
	if body.ResponseType != "Completed" && body.ResponseType != "Cancelled" {
		return &Error{Kind: Rejected, Code: "invalid_request", Message: `ResponseType must be "Completed" or "Cancelled"`}
	}
	path := PathRegisterV1
	if c.env == config.MpesaProduction {
		path = PathRegisterV2
	}
	res, ferr := c.call(ctx, op, path, body, "")
	if ferr != nil {
		return ferr.Error
	}
	if res.status != http.StatusOK {
		return statusError(res)
	}
	var r registerReply
	if err := decodeJSON(res.body, &r); err != nil {
		return &Error{Kind: Transient, HTTPStatus: res.status, Message: "unreadable register URL response"}
	}
	if code := strings.TrimSpace(scalar(r.ResponseCode)); strings.Trim(code, "0") != "" {
		return &Error{Kind: Rejected, HTTPStatus: res.status, Code: clean(code), Message: clean(r.ResponseDescription)}
	}
	return nil
}

// ---------------------------------------------------------------- transport

type response struct {
	status int
	body   []byte
}

// callError is a failure before any API response was received. notSent is
// set when the request certainly never reached Daraja.
type callError struct {
	*Error
	notSent bool
}

// call POSTs body as JSON with a bearer token. A 401 invalidates the token
// and retries once. It returns an error only when no HTTP response was
// received.
func (c *HTTPClient) call(ctx context.Context, op, path string, body any, phone string) (response, *callError) {
	payload, err := json.Marshal(body)
	if err != nil {
		return response{}, &callError{&Error{Kind: Rejected, Message: "cannot encode request"}, true}
	}
	for attempt := 0; ; attempt++ {
		tok, terr := c.accessToken(ctx)
		if terr != nil {
			cp := *terr // shared between singleflight waiters: copy
			if cp.Message == "" {
				cp.Message = "could not get an access token"
			}
			return response{}, &callError{&cp, true}
		}
		res, ferr := c.do(ctx, op, http.MethodPost, path, payload, "Bearer "+tok, phone)
		if ferr != nil {
			return response{}, ferr
		}
		if res.status == http.StatusUnauthorized && attempt == 0 {
			c.invalidate(tok)
			continue
		}
		return res, nil
	}
}

// do performs one HTTP request with the per-call timeout and logs it
// (operation, status, duration, masked phone; never headers or bodies).
func (c *HTTPClient) do(ctx context.Context, op, method, path string, payload []byte, auth, phone string) (response, *callError) {
	if err := ctx.Err(); err != nil {
		return response{}, &callError{&Error{Kind: Transient, Err: err}, true}
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return response{}, &callError{&Error{Kind: Rejected, Message: "cannot build request"}, true}
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	start := time.Now()
	attrs := []slog.Attr{slog.String("op", op)}
	if phone != "" {
		attrs = append(attrs, slog.String("phone", tlog.MaskPhone(phone)))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		e := &Error{Kind: Transient, Err: sanitizeNetErr(err)}
		attrs = append(attrs, slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.String("error", e.Error()))
		c.log.LogAttrs(ctx, slog.LevelWarn, "mpesa call failed", attrs...)
		return response{}, &callError{e, dialFailed(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	attrs = append(attrs, slog.Int("http_status", resp.StatusCode), slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	if err != nil {
		attrs = append(attrs, slog.String("error", "reading response failed"))
		c.log.LogAttrs(ctx, slog.LevelWarn, "mpesa call failed", attrs...)
		return response{}, &callError{&Error{Kind: Transient, HTTPStatus: resp.StatusCode, Message: "reading response failed"}, false}
	}
	lvl := slog.LevelInfo
	if op == "oauth" {
		lvl = slog.LevelDebug
	}
	if resp.StatusCode >= 400 {
		lvl = slog.LevelWarn
		if de := darajaError(b); de.code != "" {
			attrs = append(attrs, slog.String("daraja_code", de.code))
		}
	}
	c.log.LogAttrs(ctx, lvl, "mpesa call", attrs...)
	return response{status: resp.StatusCode, body: b}, nil
}

// dialFailed reports whether err happened while connecting (so nothing was
// sent).
func dialFailed(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// sanitizeNetErr keeps the cause of a transport error but drops the URL
// wrapper (the URLs carry no secrets, but keep errors short and stable).
func sanitizeNetErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if errors.Is(ue.Err, errRedirect) {
			return errRedirect
		}
		if ue.Timeout() {
			return context.DeadlineExceeded
		}
		return ue.Err
	}
	return err
}

// statusError classifies a non-200 response. 4xx (except 408 and 429) is a
// rejection; everything else is transient. For STK push unexpected statuses
// (3xx, 1xx) are transient too: the outcome is unknown.
func statusError(res response) *Error {
	de := darajaError(res.body)
	kind := Transient
	if res.status >= 400 && res.status < 500 && res.status != http.StatusRequestTimeout && res.status != http.StatusTooManyRequests {
		kind = Rejected
	}
	return &Error{Kind: kind, HTTPStatus: res.status, Code: de.code, Message: de.message}
}

type darajaErr struct{ code, message string }

// darajaError extracts {"errorCode","errorMessage"} (or the fault shape
// Apigee uses for spike arrest) from an error body.
func darajaError(b []byte) darajaErr {
	var v struct {
		ErrorCode    json.RawMessage `json:"errorCode"`
		ErrorMessage string          `json:"errorMessage"`
		Fault        *struct {
			FaultString string `json:"faultstring"`
			Detail      struct {
				ErrorCode string `json:"errorcode"`
			} `json:"detail"`
		} `json:"fault"`
	}
	if json.Unmarshal(b, &v) != nil {
		return darajaErr{}
	}
	code := strings.Trim(strings.TrimSpace(string(v.ErrorCode)), `"`)
	msg := v.ErrorMessage
	if v.Fault != nil {
		code = firstNonEmpty(code, v.Fault.Detail.ErrorCode)
		msg = firstNonEmpty(msg, v.Fault.FaultString)
	}
	return darajaErr{code: truncate(clean(code), 64), message: clean(msg)}
}

func decodeJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	return dec.Decode(v)
}

// clean trims, drops control characters and caps the length of a string
// taken from a Daraja response.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.TrimSpace(s))
	return truncate(s, maxMessageLen)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// ---------------------------------------------------------------- OAuth

type tokenReply struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   any    `json:"expires_in"` // Daraja sends a string ("3599")
}

// accessToken returns a cached token, fetching one if needed. Concurrent
// callers share one fetch; each waits only as long as its own ctx allows.
func (c *HTTPClient) accessToken(ctx context.Context) (string, *Error) {
	if tok := c.cachedToken(); tok != "" {
		return tok, nil
	}
	ch := c.sf.DoChan("token", func() (any, error) {
		if tok := c.cachedToken(); tok != "" {
			return tok, nil
		}
		// Detached from the first caller's ctx so its cancellation does not
		// fail every waiter.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		defer cancel()
		tok, err := c.fetchToken(fctx)
		if err != nil {
			return nil, err
		}
		return tok, nil
	})
	select {
	case r := <-ch:
		if r.Err != nil {
			var e *Error
			if errors.As(r.Err, &e) {
				return "", e
			}
			return "", &Error{Kind: Transient, Err: r.Err}
		}
		tok, _ := r.Val.(string)
		return tok, nil
	case <-ctx.Done():
		return "", &Error{Kind: Transient, Err: ctx.Err()}
	}
}

func (c *HTTPClient) cachedToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.refreshAt) {
		return c.token
	}
	return ""
}

// invalidate drops tok if it is still the cached token.
func (c *HTTPClient) invalidate(tok string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == tok {
		c.token = ""
	}
}

func (c *HTTPClient) fetchToken(ctx context.Context) (string, *Error) {
	basic := base64.StdEncoding.EncodeToString([]byte(c.key.Reveal() + ":" + c.secret.Reveal()))
	res, ferr := c.do(ctx, "oauth", http.MethodGet, PathOAuth+"?grant_type=client_credentials", nil, "Basic "+basic, "")
	if ferr != nil {
		return "", ferr.Error
	}
	if res.status != http.StatusOK {
		e := statusError(res)
		if e.Message == "" {
			e.Message = "access token request failed"
		}
		return "", e
	}
	var r tokenReply
	if err := decodeJSON(res.body, &r); err != nil || strings.TrimSpace(r.AccessToken) == "" {
		return "", &Error{Kind: Transient, HTTPStatus: res.status, Message: "unreadable access token response"}
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(scalar(r.ExpiresIn)), 10, 64)
	if err != nil || secs <= 0 {
		secs = 3599 // Daraja's documented lifetime
	}
	ttl := time.Duration(secs) * time.Second
	// Refresh ~5 minutes early; short-lived tokens at half their life.
	early := 5 * time.Minute
	if ttl < 2*early {
		early = ttl / 2
	}
	tok := strings.TrimSpace(r.AccessToken)
	c.mu.Lock()
	c.token = tok
	c.refreshAt = c.now().Add(ttl - early)
	c.mu.Unlock()
	return tok, nil
}
