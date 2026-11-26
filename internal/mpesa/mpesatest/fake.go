// Package mpesatest is an in-memory fake of Safaricom Daraja for tests,
// smoke runs and the development stack (cmd/fakedaraja). It serves the same
// endpoints the real client uses, with Daraja's request and response shapes:
//
//	GET  /oauth/v1/generate?grant_type=client_credentials   (Basic auth)
//	POST /mpesa/stkpush/v1/processrequest                   (Bearer)
//	POST /mpesa/stkpushquery/v1/query                       (Bearer)
//	POST /mpesa/c2b/v1/registerurl, /mpesa/c2b/v2/registerurl (Bearer)
//
// plus test helpers that real Daraja does not have:
//
//	POST /fake/c2b     {"bill_ref":"HOST-1042","amount":100,"phone":"254712345678"}
//	                   posts a C2B confirmation to the registered URL
//	GET  /fake/pushes  JSON snapshot of every push received
//	GET  /healthz
//
// # Behaviour by phone number
//
// What happens to an STK push is chosen by the LAST digit of PhoneNumber
// (override per phone with SetBehaviour):
//
//	0 (and any other digit) Success: callback ResultCode 0 with a fresh
//	                        10-character receipt and the requested amount,
//	                        CallbackDelay (default 300ms) after the push.
//	1 Cancelled:            callback ResultCode 1032 "Request cancelled by user".
//	2 QueryOnlySuccess:     NO callback; STK query answers ResultCode 0
//	                        (exercises needs_review: success without receipt).
//	3 PendingForever:       no callback; STK query always answers HTTP 500
//	                        errorCode 500.001.1001 "The transaction is being processed".
//	4 RejectPush:           the push is refused: HTTP 400 errorCode 400.002.02;
//	                        nothing is recorded.
//	5 PushError500:         the push answers HTTP 500 (outcome unknown to the
//	                        caller) but IS recorded, and a success callback
//	                        follows as for 0.
//	6 DuplicateCallback:    success, and the identical callback is sent twice.
//
// Until CallbackDelay has passed, an STK query answers "being processed";
// afterwards it reports the push's outcome, whether or not the callback was
// delivered (HoldCallbacks keeps callbacks back to time races).
//
// Credentials: when Options.ConsumerKey/ConsumerSecret are set they must
// match; otherwise any non-empty Basic credentials get a token. When
// Options.Passkey is set the STK password is verified exactly; otherwise only
// its shape (base64 of shortcode + anything + timestamp) is checked.
package mpesatest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/internal/mpesa"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
)

// Behaviour is what the fake does with a push.
type Behaviour int

// Behaviours, numbered by the phone's last digit that selects them.
const (
	Success           Behaviour = 0
	Cancelled         Behaviour = 1
	QueryOnlySuccess  Behaviour = 2
	PendingForever    Behaviour = 3
	RejectPush        Behaviour = 4
	PushError500      Behaviour = 5
	DuplicateCallback Behaviour = 6
)

func (b Behaviour) String() string {
	switch b {
	case Success:
		return "success"
	case Cancelled:
		return "cancelled"
	case QueryOnlySuccess:
		return "query_only_success"
	case PendingForever:
		return "pending_forever"
	case RejectPush:
		return "reject_push"
	case PushError500:
		return "push_error_500"
	case DuplicateCallback:
		return "duplicate_callback"
	}
	return fmt.Sprintf("Behaviour(%d)", int(b))
}

// callbacks is how many STK callbacks the behaviour sends.
func (b Behaviour) callbacks() int {
	switch b {
	case Success, Cancelled, PushError500:
		return 1
	case DuplicateCallback:
		return 2
	}
	return 0
}

// BehaviourFor is the default behaviour for a phone (its last digit).
func BehaviourFor(phone string) Behaviour {
	if phone == "" {
		return Success
	}
	switch d := phone[len(phone)-1]; d {
	case '1', '2', '3', '4', '5', '6':
		return Behaviour(d - '0')
	}
	return Success
}

// Options configures the fake. The zero value works.
type Options struct {
	// CallbackDelay is how long after a push its outcome is decided and the
	// callback is sent (default 300ms).
	CallbackDelay time.Duration
	// TokenTTL is the advertised access token lifetime (default 3599s).
	TokenTTL time.Duration
	// Optional credentials to verify (see the package doc).
	ConsumerKey, ConsumerSecret, Passkey string
	// HTTPClient posts callbacks (default: 10s timeout).
	HTTPClient *http.Client
	Logger     *slog.Logger
	Now        func() time.Time
}

// Push is one STK push the fake received.
type Push struct {
	MerchantRequestID string
	CheckoutRequestID string
	ShortCode         string
	Phone             string
	Amount            int64
	AccountReference  string
	TransactionDesc   string
	CallBackURL       string
	Timestamp         string
	Behaviour         Behaviour
	CreatedAt         time.Time
	Receipt           string // set for behaviours that succeed
	// CallbacksSent counts callbacks POSTed; CallbackStatus is the HTTP
	// status of the last one (0 if it failed or none was sent).
	CallbacksSent  int
	CallbackStatus int
	Held           bool // waiting for Release (HoldCallbacks)
}

// RegisteredURLs is the last C2B registration.
type RegisteredURLs struct {
	ShortCode       string
	ResponseType    string
	ConfirmationURL string
	ValidationURL   string
	Version         string // "v1" or "v2"
}

type push struct {
	Push
	decideAt time.Time
	sent     bool // callbacks dispatched (by timer, Release or flush)
}

// Fake is the Daraja fake as an http.Handler. Use NewServer (tests) or
// NewFake + your own listener (cmd/fakedaraja).
type Fake struct {
	opts   Options
	hc     *http.Client
	log    *slog.Logger
	now    func() time.Time
	mux    *http.ServeMux
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu         sync.Mutex
	tokens     map[string]time.Time
	tokenCalls int
	pushes     []*push
	byID       map[string]*push
	overrides  map[string]Behaviour
	hold       bool
	registered *RegisteredURLs
	receipts   map[string]bool
}

// NewFake builds the handler. Call Close to stop pending callbacks.
func NewFake(o Options) *Fake {
	if o.CallbackDelay <= 0 {
		o.CallbackDelay = 300 * time.Millisecond
	}
	if o.TokenTTL <= 0 {
		o.TokenTTL = 3599 * time.Second
	}
	f := &Fake{
		opts: o, hc: o.HTTPClient, log: o.Logger, now: o.Now,
		tokens: map[string]time.Time{}, byID: map[string]*push{}, overrides: map[string]Behaviour{}, receipts: map[string]bool{},
	}
	if f.hc == nil {
		f.hc = &http.Client{Timeout: 10 * time.Second}
	}
	if f.log == nil {
		f.log = slog.New(slog.DiscardHandler)
	}
	if f.now == nil {
		f.now = time.Now
	}
	f.ctx, f.cancel = context.WithCancel(context.Background())
	m := http.NewServeMux()
	m.HandleFunc("GET /oauth/v1/generate", f.handleToken)
	m.HandleFunc("POST /mpesa/stkpush/v1/processrequest", f.auth(f.handlePush))
	m.HandleFunc("POST /mpesa/stkpushquery/v1/query", f.auth(f.handleQuery))
	m.HandleFunc("POST /mpesa/c2b/v1/registerurl", f.auth(f.handleRegister("v1")))
	m.HandleFunc("POST /mpesa/c2b/v2/registerurl", f.auth(f.handleRegister("v2")))
	m.HandleFunc("POST /fake/c2b", f.handleFakeC2B)
	m.HandleFunc("GET /fake/pushes", f.handleFakePushes)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	f.mux = m
	return f
}

// ServeHTTP implements http.Handler.
func (f *Fake) ServeHTTP(w http.ResponseWriter, r *http.Request) { f.mux.ServeHTTP(w, r) }

// Close cancels pending callbacks and waits for in-flight ones.
func (f *Fake) Close() {
	f.cancel()
	f.wg.Wait()
}

// Server is a Fake on an httptest server.
type Server struct {
	*Fake
	URL string // base URL ending in "/" (use as ClientOptions.BaseURL)
	srv *httptest.Server
}

// NewServer starts a fake on a loopback port.
func NewServer(o Options) *Server {
	f := NewFake(o)
	srv := httptest.NewServer(f)
	return &Server{Fake: f, URL: srv.URL + "/", srv: srv}
}

// New starts a fake closed by t.Cleanup.
func New(t testing.TB, o ...Options) *Server {
	t.Helper()
	var opts Options
	if len(o) > 0 {
		opts = o[0]
	}
	s := NewServer(opts)
	t.Cleanup(s.Close)
	return s
}

// Close stops callbacks and the server.
func (s *Server) Close() {
	s.Fake.Close()
	s.srv.Close()
}

// ---------------------------------------------------------------- controls

// Pushes returns a snapshot of every push received, oldest first.
func (f *Fake) Pushes() []Push {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Push, len(f.pushes))
	for i, p := range f.pushes {
		out[i] = p.Push
	}
	return out
}

// Push returns the push with this CheckoutRequestID.
func (f *Fake) Push(checkoutRequestID string) (Push, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.byID[checkoutRequestID]; ok {
		return p.Push, true
	}
	return Push{}, false
}

// TokenRequests counts successful OAuth token requests.
func (f *Fake) TokenRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tokenCalls
}

// ExpireTokens invalidates every issued token (API calls then get 401).
func (f *Fake) ExpireTokens() {
	f.mu.Lock()
	defer f.mu.Unlock()
	clear(f.tokens)
}

// SetBehaviour overrides the behaviour for one phone number.
func (f *Fake) SetBehaviour(phone string, b Behaviour) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.overrides[phone] = b
}

// HoldCallbacks(true) keeps callbacks back once due (Release sends them);
// HoldCallbacks(false) sends everything held, in the background.
func (f *Fake) HoldCallbacks(hold bool) {
	f.mu.Lock()
	f.hold = hold
	var flush []*push
	if !hold {
		for _, p := range f.pushes {
			if p.Held && !p.sent {
				p.sent, p.Held = true, false
				flush = append(flush, p)
			}
		}
	}
	f.mu.Unlock()
	for _, p := range flush {
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			_ = f.sendCallbacks(f.ctx, p)
		}()
	}
}

// Release sends a push's callback(s) now (held or not yet due) and returns
// the first delivery error.
func (f *Fake) Release(checkoutRequestID string) error {
	f.mu.Lock()
	p, ok := f.byID[checkoutRequestID]
	switch {
	case !ok:
		f.mu.Unlock()
		return fmt.Errorf("mpesatest: no push %q", checkoutRequestID)
	case p.Behaviour.callbacks() == 0:
		f.mu.Unlock()
		return fmt.Errorf("mpesatest: behaviour %s sends no callback", p.Behaviour)
	case p.sent:
		f.mu.Unlock()
		return fmt.Errorf("mpesatest: callback for %q already sent", checkoutRequestID)
	}
	p.sent, p.Held = true, false
	if now := f.now(); now.Before(p.decideAt) {
		p.decideAt = now // the outcome is decided once its callback is out
	}
	f.mu.Unlock()
	return f.sendCallbacks(f.ctx, p)
}

// RegisteredURLs returns the last C2B registration.
func (f *Fake) RegisteredURLs() (RegisteredURLs, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registered == nil {
		return RegisteredURLs{}, false
	}
	return *f.registered, true
}

// SimulateC2B posts a paybill confirmation (as Safaricom does when a
// customer pays the paybill with billRef as the account number) to the
// registered confirmation URL. It returns the receipt.
func (f *Fake) SimulateC2B(ctx context.Context, billRef string, amount int64, phone string) (string, error) {
	f.mu.Lock()
	reg := f.registered
	f.mu.Unlock()
	if reg == nil {
		return "", errors.New("mpesatest: no C2B URLs registered")
	}
	if amount < 1 || amount > mpesa.MaxAmount {
		return "", fmt.Errorf("mpesatest: amount must be 1-%d", mpesa.MaxAmount)
	}
	receipt := f.newReceipt()
	body := map[string]string{
		"TransactionType":   "Pay Bill",
		"TransID":           receipt,
		"TransTime":         mpesa.Timestamp(f.now()),
		"TransAmount":       strconv.FormatInt(amount, 10) + ".00",
		"BusinessShortCode": reg.ShortCode,
		"BillRefNumber":     billRef,
		"InvoiceNumber":     "",
		"OrgAccountBalance": "49197.00",
		"ThirdPartyTransID": "",
		// Safaricom now masks the payer's number in C2B confirmations.
		"MSISDN":     maskC2BPhone(phone),
		"FirstName":  "John",
		"MiddleName": "",
		"LastName":   "",
	}
	status, err := f.post(ctx, reg.ConfirmationURL, body)
	f.log.Info("fake daraja: c2b confirmation", "bill_ref", billRef, "amount", amount, "status", status, tlog.Err(err))
	if err != nil {
		return receipt, err
	}
	if status != http.StatusOK {
		return receipt, fmt.Errorf("mpesatest: confirmation URL answered HTTP %d", status)
	}
	return receipt, nil
}

func maskC2BPhone(p string) string {
	if len(p) < 8 {
		return p
	}
	return p[:4] + " ***** " + p[len(p)-3:]
}

// ---------------------------------------------------------------- handlers

func (f *Fake) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("grant_type") != "client_credentials" {
		writeErr(w, http.StatusBadRequest, "400.008.02", "Invalid grant type passed")
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user == "" || pass == "" ||
		(f.opts.ConsumerKey != "" && !eq(user, f.opts.ConsumerKey)) ||
		(f.opts.ConsumerSecret != "" && !eq(pass, f.opts.ConsumerSecret)) {
		writeErr(w, http.StatusBadRequest, "400.008.01", "Invalid Authentication passed")
		return
	}
	tok := randString(28, alnum)
	f.mu.Lock()
	f.tokens[tok] = f.now().Add(f.opts.TokenTTL)
	f.tokenCalls++
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{
		"access_token": tok,
		"expires_in":   strconv.Itoa(int(f.opts.TokenTTL / time.Second)),
	})
}

func (f *Fake) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		exp, known := f.tokens[tok]
		valid := ok && known && f.now().Before(exp)
		f.mu.Unlock()
		if !valid {
			writeErr(w, http.StatusUnauthorized, "404.001.04", "Invalid Access Token")
			return
		}
		next(w, r)
	}
}

type pushReq struct {
	BusinessShortCode any    `json:"BusinessShortCode"`
	Password          string `json:"Password"`
	Timestamp         string `json:"Timestamp"`
	TransactionType   string `json:"TransactionType"`
	Amount            any    `json:"Amount"`
	PartyA            any    `json:"PartyA"`
	PartyB            any    `json:"PartyB"`
	PhoneNumber       any    `json:"PhoneNumber"`
	CallBackURL       string `json:"CallBackURL"`
	AccountReference  string `json:"AccountReference"`
	TransactionDesc   string `json:"TransactionDesc"`
}

var (
	tsRe    = regexp.MustCompile(`^[0-9]{14}$`)
	phoneRe = regexp.MustCompile(`^254[17][0-9]{8}$`)
	scRe    = regexp.MustCompile(`^[0-9]{5,7}$`)
	wholeRe = regexp.MustCompile(`^[0-9]{1,9}$`)
)

func (f *Fake) handlePush(w http.ResponseWriter, r *http.Request) {
	var q pushReq
	if !decode(w, r, &q) {
		return
	}
	sc, phone := str(q.BusinessShortCode), str(q.PhoneNumber)
	amountStr := strings.TrimSuffix(strings.TrimSuffix(str(q.Amount), ".00"), ".0")
	amount, _ := strconv.ParseInt(amountStr, 10, 64)
	bad := func(field string) {
		writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid "+field)
	}
	switch {
	case !scRe.MatchString(sc):
		bad("BusinessShortCode")
	case !tsRe.MatchString(q.Timestamp):
		bad("Timestamp")
	case !f.passwordOK(sc, q.Timestamp, q.Password):
		bad("Password")
	case q.TransactionType != "CustomerPayBillOnline" && q.TransactionType != "CustomerBuyGoodsOnline":
		bad("TransactionType")
	case !wholeRe.MatchString(amountStr) || amount < 1 || amount > mpesa.MaxAmount:
		bad("Amount")
	case !phoneRe.MatchString(phone) || str(q.PartyA) != phone:
		bad("PhoneNumber")
	case str(q.PartyB) != sc:
		bad("PartyB")
	case !strings.HasPrefix(q.CallBackURL, "http://") && !strings.HasPrefix(q.CallBackURL, "https://"):
		bad("CallBackURL")
	case q.AccountReference == "" || len(q.AccountReference) > 12:
		bad("AccountReference")
	case q.TransactionDesc == "" || len(q.TransactionDesc) > 13:
		bad("TransactionDesc")
	default:
		f.acceptPush(w, q, sc, phone, amount)
	}
}

func (f *Fake) acceptPush(w http.ResponseWriter, q pushReq, sc, phone string, amount int64) {
	f.mu.Lock()
	b, ok := f.overrides[phone]
	if !ok {
		b = BehaviourFor(phone)
	}
	f.mu.Unlock()
	if b == RejectPush {
		f.log.Info("fake daraja: push rejected", "phone", tlog.MaskPhone(phone), "behaviour", b.String())
		writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid PhoneNumber")
		return
	}
	now := f.now()
	p := &push{Push: Push{
		MerchantRequestID: randString(5, digits) + "-" + randString(8, digits) + "-1",
		CheckoutRequestID: "ws_CO_" + now.In(mpesa.Nairobi).Format("02012006150405") + randString(9, digits),
		ShortCode:         sc, Phone: phone, Amount: amount,
		AccountReference: q.AccountReference, TransactionDesc: q.TransactionDesc,
		CallBackURL: q.CallBackURL, Timestamp: q.Timestamp, Behaviour: b, CreatedAt: now,
	}, decideAt: now.Add(f.opts.CallbackDelay)}
	if b != Cancelled && b != PendingForever {
		p.Receipt = f.newReceipt()
	}
	f.mu.Lock()
	f.pushes = append(f.pushes, p)
	f.byID[p.CheckoutRequestID] = p
	f.mu.Unlock()
	f.log.Info("fake daraja: push accepted", "phone", tlog.MaskPhone(phone), "amount", amount,
		"checkout_request_id", p.CheckoutRequestID, "behaviour", b.String())

	if b.callbacks() > 0 {
		f.wg.Add(1)
		go f.scheduleCallback(p)
	}
	if b == PushError500 {
		writeErr(w, http.StatusInternalServerError, "500.003.1001", "Internal Server Error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"MerchantRequestID":   p.MerchantRequestID,
		"CheckoutRequestID":   p.CheckoutRequestID,
		"ResponseCode":        "0",
		"ResponseDescription": "Success. Request accepted for processing",
		"CustomerMessage":     "Success. Request accepted for processing",
	})
}

func (f *Fake) scheduleCallback(p *push) {
	defer f.wg.Done()
	t := time.NewTimer(f.opts.CallbackDelay)
	defer t.Stop()
	select {
	case <-f.ctx.Done():
		return
	case <-t.C:
	}
	f.mu.Lock()
	if p.sent {
		f.mu.Unlock()
		return
	}
	if f.hold {
		p.Held = true
		f.mu.Unlock()
		return
	}
	p.sent = true
	f.mu.Unlock()
	_ = f.sendCallbacks(f.ctx, p)
}

// sendCallbacks POSTs the push's STK callback (twice for DuplicateCallback).
func (f *Fake) sendCallbacks(ctx context.Context, p *push) error {
	body := stkCallbackBody(p.Push, f.now())
	var first error
	for i := 0; i < p.Behaviour.callbacks(); i++ {
		status, err := f.post(ctx, p.CallBackURL, body)
		f.mu.Lock()
		p.CallbacksSent++
		p.CallbackStatus = status
		f.mu.Unlock()
		f.log.Info("fake daraja: stk callback", "checkout_request_id", p.CheckoutRequestID, "status", status, tlog.Err(err))
		if err == nil && status != http.StatusOK {
			err = fmt.Errorf("mpesatest: callback URL answered HTTP %d", status)
		}
		if first == nil {
			first = err
		}
	}
	return first
}

type item struct {
	Name  string `json:"Name"`
	Value any    `json:"Value,omitempty"`
}

func stkCallbackBody(p Push, now time.Time) any {
	cb := map[string]any{
		"MerchantRequestID": p.MerchantRequestID,
		"CheckoutRequestID": p.CheckoutRequestID,
	}
	if p.Behaviour == Cancelled {
		cb["ResultCode"] = 1032
		cb["ResultDesc"] = "Request cancelled by user"
	} else {
		phone, _ := strconv.ParseInt(p.Phone, 10, 64)
		ts, _ := strconv.ParseInt(mpesa.Timestamp(now), 10, 64)
		cb["ResultCode"] = 0
		cb["ResultDesc"] = "The service request is processed successfully."
		cb["CallbackMetadata"] = map[string]any{"Item": []item{
			{Name: "Amount", Value: json.Number(strconv.FormatInt(p.Amount, 10) + ".00")},
			{Name: "MpesaReceiptNumber", Value: p.Receipt},
			{Name: "Balance"},
			{Name: "TransactionDate", Value: ts},
			{Name: "PhoneNumber", Value: phone},
		}}
	}
	return map[string]any{"Body": map[string]any{"stkCallback": cb}}
}

type queryReq struct {
	BusinessShortCode any    `json:"BusinessShortCode"`
	Password          string `json:"Password"`
	Timestamp         string `json:"Timestamp"`
	CheckoutRequestID string `json:"CheckoutRequestID"`
}

func (f *Fake) handleQuery(w http.ResponseWriter, r *http.Request) {
	var q queryReq
	if !decode(w, r, &q) {
		return
	}
	sc := str(q.BusinessShortCode)
	if !scRe.MatchString(sc) || !tsRe.MatchString(q.Timestamp) || !f.passwordOK(sc, q.Timestamp, q.Password) {
		writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid Password")
		return
	}
	f.mu.Lock()
	p, ok := f.byID[q.CheckoutRequestID]
	var snap Push
	decided := false
	if ok {
		snap = p.Push
		decided = !f.now().Before(p.decideAt)
	}
	f.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid CheckoutRequestID")
		return
	}
	if !decided || snap.Behaviour == PendingForever {
		writeErr(w, http.StatusInternalServerError, "500.001.1001", "The transaction is being processed")
		return
	}
	code, desc := "0", "The service request is processed successfully."
	if snap.Behaviour == Cancelled {
		code, desc = "1032", "Request cancelled by user"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"ResponseCode":        "0",
		"ResponseDescription": "The service request has been accepted successsfully",
		"MerchantRequestID":   snap.MerchantRequestID,
		"CheckoutRequestID":   snap.CheckoutRequestID,
		"ResultCode":          code,
		"ResultDesc":          desc,
	})
}

// refusedURLWords: like the real Daraja, register refuses URLs containing
// these (case-insensitive). Kept apart from mpesa.ForbiddenURLWords on
// purpose: this side plays Safaricom.
var refusedURLWords = []string{"m-pesa", "mpesa", "safaricom", "exec", "exe", "cmd", "sql", "query"}

func urlWord(u string) string {
	l := strings.ToLower(u)
	for _, w := range refusedURLWords {
		if strings.Contains(l, w) {
			return w
		}
	}
	return ""
}

func (f *Fake) handleRegister(version string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			ShortCode       any    `json:"ShortCode"`
			ResponseType    string `json:"ResponseType"`
			ConfirmationURL string `json:"ConfirmationURL"`
			ValidationURL   string `json:"ValidationURL"`
		}
		if !decode(w, r, &q) {
			return
		}
		sc := str(q.ShortCode)
		switch {
		case !scRe.MatchString(sc):
			writeErr(w, http.StatusBadRequest, "400.003.02", "Bad Request - Invalid ShortCode")
			return
		case q.ResponseType != "Completed" && q.ResponseType != "Cancelled":
			writeErr(w, http.StatusBadRequest, "400.003.02", "Bad Request - Invalid ResponseType")
			return
		case !strings.HasPrefix(q.ConfirmationURL, "http"):
			writeErr(w, http.StatusBadRequest, "400.003.02", "Bad Request - Invalid ConfirmationURL")
			return
		case urlWord(q.ConfirmationURL) != "":
			writeErr(w, http.StatusBadRequest, "400.003.02", "Bad Request - Invalid ConfirmationURL - URL has the word "+strings.ToUpper(urlWord(q.ConfirmationURL)))
			return
		case urlWord(q.ValidationURL) != "":
			writeErr(w, http.StatusBadRequest, "400.003.02", "Bad Request - Invalid ValidationURL - URL has the word "+strings.ToUpper(urlWord(q.ValidationURL)))
			return
		}
		f.mu.Lock()
		f.registered = &RegisteredURLs{ShortCode: sc, ResponseType: q.ResponseType,
			ConfirmationURL: q.ConfirmationURL, ValidationURL: q.ValidationURL, Version: version}
		f.mu.Unlock()
		f.log.Info("fake daraja: c2b urls registered", "shortcode", sc, "version", version)
		writeJSON(w, http.StatusOK, map[string]string{
			"OriginatorCoversationID": randString(5, digits) + "-" + randString(7, digits) + "-1",
			"ResponseCode":            "0",
			"ResponseDescription":     "Success",
		})
	}
}

func (f *Fake) handleFakeC2B(w http.ResponseWriter, r *http.Request) {
	var q struct {
		BillRef string `json:"bill_ref"`
		Amount  int64  `json:"amount"`
		Phone   string `json:"phone"`
	}
	if !decode(w, r, &q) {
		return
	}
	if q.Phone == "" {
		q.Phone = "254708374149"
	}
	receipt, err := f.SimulateC2B(r.Context(), q.BillRef, q.Amount, q.Phone)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "receipt": receipt})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"receipt": receipt})
}

func (f *Fake) handleFakePushes(w http.ResponseWriter, _ *http.Request) {
	type view struct {
		Push
		Behaviour string
	}
	ps := f.Pushes()
	out := make([]view, len(ps))
	for i, p := range ps {
		out[i] = view{Push: p, Behaviour: p.Behaviour.String()}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---------------------------------------------------------------- helpers

func (f *Fake) passwordOK(shortCode, ts, pw string) bool {
	if f.opts.Passkey != "" {
		want := base64.StdEncoding.EncodeToString([]byte(shortCode + f.opts.Passkey + ts))
		return eq(pw, want)
	}
	raw, err := base64.StdEncoding.DecodeString(pw)
	s := string(raw)
	return err == nil && len(s) > len(shortCode)+len(ts) && strings.HasPrefix(s, shortCode) && strings.HasSuffix(s, ts)
}

func (f *Fake) newReceipt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		r := randString(10, upperAlnum)
		if !f.receipts[r] {
			f.receipts[r] = true
			return r
		}
	}
}

func (f *Fake) post(ctx context.Context, url string, body any) (int, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "400.002.02", "Bad Request - Invalid JSON")
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{
		"requestId":    randString(5, digits) + "-" + randString(8, digits) + "-1",
		"errorCode":    code,
		"errorMessage": msg,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// str renders a JSON scalar (string or number) as text.
func str(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return x.String()
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

func eq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

const (
	digits     = "0123456789"
	alnum      = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	upperAlnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

func randString(n int, alphabet string) string {
	b := make([]byte, n)
	n64 := big.NewInt(int64(len(alphabet)))
	for i := range b {
		k, err := rand.Int(rand.Reader, n64)
		if err != nil {
			panic(err)
		}
		b[i] = alphabet[k.Int64()]
	}
	return string(b)
}
