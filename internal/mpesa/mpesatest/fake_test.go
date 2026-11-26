package mpesatest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/mpesa/mpesatest"
)

const cbToken = "tok_0123456789abcdefghijklmnopqrstuvwxyzABCD"

type received struct {
	path string
	body []byte
}

// receiver plays the Talksasa callback endpoints.
type receiver struct {
	srv *httptest.Server
	ch  chan received
	mu  sync.Mutex
	all []received
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{ch: make(chan received, 100)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		rc := received{path: req.URL.Path, body: b}
		r.mu.Lock()
		r.all = append(r.all, rc)
		r.mu.Unlock()
		r.ch <- rc
		_, _ = io.WriteString(w, `{"ResultCode":0,"ResultDesc":"Accepted"}`)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) next(t *testing.T) received {
	t.Helper()
	select {
	case rc := <-r.ch:
		return rc
	case <-time.After(3 * time.Second):
		t.Fatal("no callback")
		return received{}
	}
}

func (r *receiver) none(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case rc := <-r.ch:
		t.Fatalf("unexpected callback %s %s", rc.path, rc.body)
	case <-time.After(d):
	}
}

const delay = 30 * time.Millisecond

func setup(t *testing.T, o ...mpesatest.Options) (*mpesatest.Server, *mpesa.HTTPClient, *receiver) {
	t.Helper()
	opts := mpesatest.Options{CallbackDelay: delay}
	if len(o) > 0 {
		opts = o[0]
	}
	fake := mpesatest.New(t, opts)
	rcv := newReceiver(t)
	c, err := mpesa.NewClient(mpesa.ClientOptions{
		Env: config.MpesaSandbox, BaseURL: fake.URL, ShortCode: "174379",
		ConsumerKey: config.NewSecret("key"), ConsumerSecret: config.NewSecret("secret"),
		Passkey: config.NewSecret("passkey"), CallbackToken: config.NewSecret(cbToken),
		CallbackBaseURL: rcv.srv.URL + "/", Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return fake, c, rcv
}

func push(t *testing.T, c *mpesa.HTTPClient, phone string, amount int64) (mpesa.STKPushResponse, error) {
	t.Helper()
	return c.STKPush(context.Background(), mpesa.STKPushRequest{Phone: phone, Amount: amount, AccountReference: "HOST-1042", TransactionDesc: "PR-1"})
}

func query(t *testing.T, c *mpesa.HTTPClient, id string) mpesa.STKQueryResult {
	t.Helper()
	r, err := c.STKQuery(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBehaviourFor(t *testing.T) {
	want := map[string]mpesatest.Behaviour{
		"254708374140": mpesatest.Success, "254708374141": mpesatest.Cancelled, "254708374142": mpesatest.QueryOnlySuccess,
		"254708374143": mpesatest.PendingForever, "254708374144": mpesatest.RejectPush, "254708374145": mpesatest.PushError500,
		"254708374146": mpesatest.DuplicateCallback, "254708374147": mpesatest.Success, "254708374149": mpesatest.Success,
	}
	for p, b := range want {
		if got := mpesatest.BehaviourFor(p); got != b {
			t.Errorf("%s: %s, want %s", p, got, b)
		}
	}
}

func TestSuccess(t *testing.T) {
	fake, c, rcv := setup(t)
	res, err := push(t, c, "254708374140", 1500)
	if err != nil {
		t.Fatal(err)
	}
	if q := query(t, c, res.CheckoutRequestID); q.State != mpesa.QueryPending {
		t.Fatalf("before callback: %+v", q)
	}
	rc := rcv.next(t)
	if rc.path != "/cb/hooks/"+cbToken+"/stk" {
		t.Fatalf("path %s", rc.path)
	}
	r, err := mpesa.ParseSTKCallback(rc.body)
	if err != nil {
		t.Fatalf("%v: %s", err, rc.body)
	}
	p, _ := fake.Push(res.CheckoutRequestID)
	if !r.Succeeded() || r.Amount != 1500 || r.Receipt != p.Receipt || len(r.Receipt) != 10 ||
		r.CheckoutRequestID != res.CheckoutRequestID || r.MerchantRequestID != res.MerchantRequestID ||
		r.Phone != "254708374140" || r.OccurredAt == nil {
		t.Fatalf("%+v", r)
	}
	if q := query(t, c, res.CheckoutRequestID); q.State != mpesa.QuerySucceeded {
		t.Fatalf("after callback: %+v", q)
	}
	rcv.none(t, 3*delay)
	ps := fake.Pushes()
	if len(ps) != 1 || ps[0].CallbacksSent != 1 || ps[0].CallbackStatus != 200 || ps[0].AccountReference != "HOST-1042" {
		t.Fatalf("%+v", ps)
	}
}

func TestCancelled(t *testing.T) {
	_, c, rcv := setup(t)
	res, err := push(t, c, "254708374141", 10)
	if err != nil {
		t.Fatal(err)
	}
	r, err := mpesa.ParseSTKCallback(rcv.next(t).body)
	if err != nil || r.Succeeded() || r.ResultCode != mpesa.ResultCancelledByUser || r.ResultDesc != "Request cancelled by user" {
		t.Fatalf("%+v %v", r, err)
	}
	if q := query(t, c, res.CheckoutRequestID); q.State != mpesa.QueryFailed || q.ResultCode != "1032" {
		t.Fatalf("%+v", q)
	}
}

func TestQueryOnlySuccess(t *testing.T) {
	_, c, rcv := setup(t)
	res, err := push(t, c, "254708374142", 10)
	if err != nil {
		t.Fatal(err)
	}
	rcv.none(t, 4*delay)
	if q := query(t, c, res.CheckoutRequestID); q.State != mpesa.QuerySucceeded || q.ResultCode != "0" {
		t.Fatalf("%+v", q)
	}
}

func TestPendingForever(t *testing.T) {
	_, c, rcv := setup(t)
	res, err := push(t, c, "254708374143", 10)
	if err != nil {
		t.Fatal(err)
	}
	rcv.none(t, 4*delay)
	if q := query(t, c, res.CheckoutRequestID); q.State != mpesa.QueryPending {
		t.Fatalf("%+v", q)
	}
}

func TestRejectPush(t *testing.T) {
	fake, c, rcv := setup(t)
	_, err := push(t, c, "254708374144", 10)
	var e *mpesa.Error
	if !mpesa.IsRejected(err) || !asErr(err, &e) || e.HTTPStatus != 400 || e.Code != "400.002.02" {
		t.Fatalf("%v", err)
	}
	if len(fake.Pushes()) != 0 {
		t.Fatal("rejected push recorded")
	}
	rcv.none(t, 3*delay)
}

func TestPushError500StillCallsBack(t *testing.T) {
	fake, c, rcv := setup(t)
	_, err := push(t, c, "254708374145", 77)
	if !mpesa.IsTransient(err) {
		t.Fatalf("%v", err)
	}
	ps := fake.Pushes()
	if len(ps) != 1 {
		t.Fatalf("%+v", ps)
	}
	r, err := mpesa.ParseSTKCallback(rcv.next(t).body)
	if err != nil || !r.Succeeded() || r.Amount != 77 || r.CheckoutRequestID != ps[0].CheckoutRequestID {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestDuplicateCallback(t *testing.T) {
	_, c, rcv := setup(t)
	if _, err := push(t, c, "254708374146", 10); err != nil {
		t.Fatal(err)
	}
	a, b := rcv.next(t), rcv.next(t)
	if !bytes.Equal(a.body, b.body) {
		t.Fatalf("callbacks differ:\n%s\n%s", a.body, b.body)
	}
	rcv.none(t, 3*delay)
}

func TestSetBehaviourHoldAndRelease(t *testing.T) {
	fake, c, rcv := setup(t)
	fake.SetBehaviour("254708374144", mpesatest.Success) // would reject by digit
	fake.HoldCallbacks(true)
	res, err := push(t, c, "254708374144", 10)
	if err != nil {
		t.Fatal(err)
	}
	res2, err := push(t, c, "254711111110", 20)
	if err != nil {
		t.Fatal(err)
	}
	rcv.none(t, 4*delay)
	if p, _ := fake.Push(res.CheckoutRequestID); !p.Held || p.CallbacksSent != 0 {
		t.Fatalf("%+v", p)
	}
	// The outcome is decided even while the callback is held.
	if q := query(t, c, res.CheckoutRequestID); q.State != mpesa.QuerySucceeded {
		t.Fatalf("%+v", q)
	}
	if err := fake.Release(res.CheckoutRequestID); err != nil {
		t.Fatal(err)
	}
	if r, _ := mpesa.ParseSTKCallback(rcv.next(t).body); r.CheckoutRequestID != res.CheckoutRequestID {
		t.Fatalf("%+v", r)
	}
	if err := fake.Release(res.CheckoutRequestID); err == nil {
		t.Fatal("second release should fail")
	}
	fake.HoldCallbacks(false) // flushes the other held push
	if r, _ := mpesa.ParseSTKCallback(rcv.next(t).body); r.CheckoutRequestID != res2.CheckoutRequestID {
		t.Fatalf("%+v", r)
	}
}

func TestReleaseBeforeDue(t *testing.T) {
	fake, c, rcv := setup(t, mpesatest.Options{CallbackDelay: time.Hour})
	res, err := push(t, c, "254708374140", 10)
	if err != nil {
		t.Fatal(err)
	}
	if q := query(t, c, res.CheckoutRequestID); q.State != mpesa.QueryPending {
		t.Fatalf("%+v", q)
	}
	if err := fake.Release(res.CheckoutRequestID); err != nil {
		t.Fatal(err)
	}
	rcv.next(t)
	if q := query(t, c, res.CheckoutRequestID); q.State != mpesa.QuerySucceeded {
		t.Fatalf("%+v", q)
	}
}

func TestC2B(t *testing.T) {
	fake, c, rcv := setup(t)
	if _, err := fake.SimulateC2B(context.Background(), "HOST-1", 10, "254708374149"); err == nil {
		t.Fatal("want error before registration")
	}
	if err := c.RegisterC2BURLs(context.Background(), mpesa.RegisterURLsRequest{}); err != nil {
		t.Fatal(err)
	}
	reg, ok := fake.RegisteredURLs()
	_, conf, _ := c.CallbackURLs()
	if !ok || reg.ConfirmationURL != conf || reg.ShortCode != "174379" || reg.Version != "v1" || reg.ResponseType != "Completed" {
		t.Fatalf("%+v", reg)
	}
	receipt, err := fake.SimulateC2B(context.Background(), "host-1042", 2500, "254708374149")
	if err != nil {
		t.Fatal(err)
	}
	rc := rcv.next(t)
	if rc.path != "/cb/hooks/"+cbToken+"/c2b/confirmation" {
		t.Fatalf("path %s", rc.path)
	}
	cf, err := mpesa.ParseC2BConfirmation(rc.body)
	if err != nil || cf.Receipt != receipt || cf.Amount != 2500 || cf.BillRef != "host-1042" || cf.ShortCode != "174379" ||
		cf.Phone != "" || cf.OccurredAt == nil || cf.TransType != "Pay Bill" {
		t.Fatalf("%+v %v", cf, err)
	}

	// The HTTP helper used by smoke tests.
	resp, err := http.Post(fake.URL+"fake/c2b", "application/json", strings.NewReader(`{"bill_ref":"HOST-7","amount":5,"phone":"254712345678"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Receipt string }
	_ = json.NewDecoder(resp.Body).Decode(&out)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(out.Receipt) != 10 {
		t.Fatalf("%d %+v", resp.StatusCode, out)
	}
	if cf, err := mpesa.ParseC2BConfirmation(rcv.next(t).body); err != nil || cf.Receipt != out.Receipt || cf.BillRef != "HOST-7" {
		t.Fatalf("%+v %v", cf, err)
	}
}

func TestTokensAndExpiry(t *testing.T) {
	fake, c, _ := setup(t)
	if _, err := push(t, c, "254708374143", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := push(t, c, "254708374143", 10); err != nil {
		t.Fatal(err)
	}
	if n := fake.TokenRequests(); n != 1 {
		t.Fatalf("tokens %d", n)
	}
	fake.ExpireTokens()
	if _, err := push(t, c, "254708374143", 10); err != nil { // 401 -> refetch -> retry
		t.Fatal(err)
	}
	if n := fake.TokenRequests(); n != 2 {
		t.Fatalf("tokens %d", n)
	}
}

func TestCredentialAndPasswordChecks(t *testing.T) {
	_, c, _ := setup(t, mpesatest.Options{CallbackDelay: delay, ConsumerKey: "key", ConsumerSecret: "secret", Passkey: "passkey"})
	if _, err := push(t, c, "254708374143", 10); err != nil {
		t.Fatal(err)
	}
	_, wrong, _ := setup(t, mpesatest.Options{CallbackDelay: delay, ConsumerKey: "key", ConsumerSecret: "secret", Passkey: "other"})
	if _, err := push(t, wrong, "254708374143", 10); !mpesa.IsRejected(err) {
		t.Fatalf("wrong passkey: %v", err)
	}
	_, badCreds, _ := setup(t, mpesatest.Options{CallbackDelay: delay, ConsumerKey: "nope", ConsumerSecret: "secret"})
	if _, err := push(t, badCreds, "254708374143", 10); !mpesa.IsRejected(err) {
		t.Fatalf("wrong credentials: %v", err)
	}
}

func TestUnknownCheckoutID(t *testing.T) {
	_, c, _ := setup(t)
	if _, err := c.STKQuery(context.Background(), "ws_CO_nope"); !mpesa.IsRejected(err) {
		t.Fatalf("%v", err)
	}
}

func TestConcurrentPushes(t *testing.T) {
	fake, c, rcv := setup(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			phone := "25470837414" + string(rune('0'+i%2*7)) // ...0 or ...7: both succeed
			if _, err := push(t, c, phone, int64(i+1)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for range 20 {
		r, err := mpesa.ParseSTKCallback(rcv.next(t).body)
		if err != nil || seen[r.Receipt] {
			t.Fatalf("%+v %v", r, err)
		}
		seen[r.Receipt] = true
	}
	if len(fake.Pushes()) != 20 {
		t.Fatal(len(fake.Pushes()))
	}
}

func asErr(err error, target **mpesa.Error) bool {
	return errors.As(err, target)
}
