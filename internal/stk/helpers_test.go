package stk

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/seed"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

const testShortCode = "174379"

var operator = audit.CLI("stk-test")

// fakeDaraja is an in-memory mpesa.Client the tests control.
type fakeDaraja struct {
	mu       sync.Mutex
	pushErr  error // returned by STKPush (nil: accepted)
	pushes   []mpesa.STKPushRequest
	checkout []string
	query    map[string]mpesa.STKQueryResult
	queryErr error
	queries  int
	n        int
}

func newFake() *fakeDaraja { return &fakeDaraja{query: map[string]mpesa.STKQueryResult{}} }

func (f *fakeDaraja) STKPush(_ context.Context, req mpesa.STKPushRequest) (mpesa.STKPushResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	f.pushes = append(f.pushes, req)
	co := fmt.Sprintf("ws_CO_%s_%d", strings.ReplaceAll(uuid.NewString(), "-", "")[:10], f.n)
	f.checkout = append(f.checkout, co)
	if f.pushErr != nil {
		return mpesa.STKPushResponse{}, f.pushErr
	}
	return mpesa.STKPushResponse{MerchantRequestID: fmt.Sprintf("mr-%d", f.n), CheckoutRequestID: co, ResponseCode: "0"}, nil
}

func (f *fakeDaraja) STKQuery(_ context.Context, checkoutRequestID string) (mpesa.STKQueryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queries++
	if f.queryErr != nil {
		return mpesa.STKQueryResult{}, f.queryErr
	}
	r, ok := f.query[checkoutRequestID]
	if !ok {
		return mpesa.STKQueryResult{State: mpesa.QueryPending}, nil
	}
	return r, nil
}

func (f *fakeDaraja) RegisterC2BURLs(context.Context, mpesa.RegisterURLsRequest) error { return nil }

func (f *fakeDaraja) setQuery(checkout string, r mpesa.STKQueryResult) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.query[checkout] = r
}

func (f *fakeDaraja) setPushErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushErr = err
}

func (f *fakeDaraja) lastPush() (mpesa.STKPushRequest, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushes[len(f.pushes)-1], f.checkout[len(f.checkout)-1]
}

type env struct {
	t        *testing.T
	ctx      context.Context
	db       *testdb.DB
	logs     *syncBuffer
	fake     *fakeDaraja
	svc      *Service
	apps     *app.Service
	ledger   *ledger.Service
	prefixes map[uuid.UUID]string
	offset   atomic.Int64 // clock offset, ns
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := testdb.New(t)
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	kr, err := crypto.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(key), 0)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := queue.NewInsertOnly(d.App, logger)
	if err != nil {
		t.Fatal(err)
	}
	enq := queue.NewEnqueuer(rc)
	ev := events.NewEmitter(apiview.View{PayBaseURL: "https://pay.test/", Paybill: testShortCode}, enq)
	led := ledger.NewService(d.App, logger)
	e := &env{
		t: t, ctx: context.Background(), db: d, logs: logs, fake: newFake(),
		apps: app.NewService(app.Options{
			Pool: d.App, Keyring: kr, APIKeyPepper: []byte(strings.Repeat("p", 40)), AllowHTTPHooks: true, Logger: logger,
		}),
		ledger:   led,
		prefixes: map[uuid.UUID]string{},
	}
	e.svc = NewService(Options{
		Pool: d.App, Client: e.fake, Ledger: led, Events: ev, Callbacks: enq, Logger: logger,
		Now: e.now, ShortCode: testShortCode,
	})
	return e
}

func (e *env) now() time.Time { return time.Now().Add(time.Duration(e.offset.Load())) }

// advance moves the service clock forward.
func (e *env) advance(d time.Duration) { e.offset.Add(int64(d)) }

func (e *env) app(prefix string) uuid.UUID {
	e.t.Helper()
	c, err := e.apps.Create(e.ctx, operator, app.CreateParams{
		Name: prefix + " app", AccountPrefix: prefix, WebhookURL: "https://" + strings.ToLower(prefix) + ".example.com/hooks",
	})
	if err != nil {
		e.t.Fatalf("create app %s: %v", prefix, err)
	}
	e.prefixes[c.App.ID] = prefix
	return c.App.ID
}

// charge seeds a pending charge of the app (store queries; the charge
// service is not under test here).
func (e *env) charge(appID uuid.UUID, amount int64) store.Charge {
	e.t.Helper()
	return seed.Charge(e.t, e.ctx, e.db.App, appID, e.prefixes[appID], amount, seed.ChargeOpts{})
}

// chargeState reloads a charge.
func (e *env) chargeState(appID, id uuid.UUID) store.Charge {
	e.t.Helper()
	c, err := store.New(e.db.App).GetChargeForApp(e.ctx, store.GetChargeForAppParams{AppID: appID, ID: id})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

var receiptSeq atomic.Int64

func newReceipt() string { return fmt.Sprintf("TST%07d", receiptSeq.Add(1)) }

// send sends an app push with a fresh key and expects success.
func (e *env) send(appID, chargeID uuid.UUID, ph string) Push {
	e.t.Helper()
	p, replayed, err := e.svc.Send(e.ctx, audit.App(appID), SendParams{
		AppID: appID, ChargeID: chargeID, Phone: ph, Source: SourceApp, IdempotencyKey: uuid.NewString(),
	})
	if err != nil || replayed {
		e.t.Fatalf("send: replayed=%v err=%v", replayed, err)
	}
	return p
}

func (e *env) push(id uuid.UUID) Push {
	e.t.Helper()
	p, err := store.New(e.db.App).GetSTKPushInternal(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) txn(id uuid.UUID) store.Transaction {
	e.t.Helper()
	t, err := store.New(e.db.App).GetTransactionInternal(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return t
}

// deliver stores and processes a callback, like the HTTP handler + worker.
func (e *env) deliver(kind CallbackKind, raw string) store.MpesaCallback {
	e.t.Helper()
	id, err := e.svc.StoreCallback(e.ctx, kind, netip.MustParseAddr("196.201.214.200"), []byte(raw))
	if err != nil {
		e.t.Fatalf("store callback: %v", err)
	}
	if err := e.svc.ProcessCallback(e.ctx, id); err != nil {
		e.t.Fatalf("process callback: %v", err)
	}
	return e.callback(id)
}

func (e *env) callback(id uuid.UUID) store.MpesaCallback {
	e.t.Helper()
	var cb store.MpesaCallback
	err := e.db.Owner.QueryRow(e.ctx, `SELECT id, kind, processed_at, error, attempts, transaction_id FROM mpesa_callbacks WHERE id = $1`, id).
		Scan(&cb.ID, &cb.Kind, &cb.ProcessedAt, &cb.Error, &cb.Attempts, &cb.TransactionID)
	if err != nil {
		e.t.Fatal(err)
	}
	return cb
}

func stkSuccess(checkout, receipt string, amount int64, ph string) string {
	return fmt.Sprintf(`{"Body":{"stkCallback":{"MerchantRequestID":"mr-x","CheckoutRequestID":%q,"ResultCode":0,
"ResultDesc":"The service request is processed successfully.","CallbackMetadata":{"Item":[
{"Name":"Amount","Value":%d},{"Name":"MpesaReceiptNumber","Value":%q},{"Name":"Balance"},
{"Name":"TransactionDate","Value":20261124101010},{"Name":"PhoneNumber","Value":%s}]}}}}`, checkout, amount, receipt, ph)
}

func stkFailure(checkout, code, desc string) string {
	return fmt.Sprintf(`{"Body":{"stkCallback":{"MerchantRequestID":"mr-x","CheckoutRequestID":%q,"ResultCode":%s,"ResultDesc":%q}}}`, checkout, code, desc)
}

func c2b(receipt string, amount int64, billRef, shortCode string) string {
	return fmt.Sprintf(`{"TransactionType":"Pay Bill","TransID":%q,"TransTime":"20261124101010","TransAmount":"%d.00",
"BusinessShortCode":%q,"BillRefNumber":%q,"InvoiceNumber":"","OrgAccountBalance":"","ThirdPartyTransID":"",
"MSISDN":"254712345678","FirstName":"JOHN"}`, receipt, amount, shortCode, billRef)
}

func (e *env) count(sql string, args ...any) int64 {
	e.t.Helper()
	var n int64
	if err := e.db.Owner.QueryRow(e.ctx, sql, args...).Scan(&n); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e *env) zero(what, sql string, args ...any) {
	e.t.Helper()
	if n := e.count(sql, args...); n != 0 {
		e.t.Fatalf("invariant %q violated: %d", what, n)
	}
}

func (e *env) events(subject uuid.UUID, typ string) int64 {
	e.t.Helper()
	return e.count(`SELECT count(*) FROM events WHERE subject_id = $1 AND type = $2`, subject, typ)
}

// checkInvariants proves the money rules hold after a test.
// TODO(gateway-core): extend with the charge invariants of the plan
// (each succeeded charge has exactly one succeeded transaction for its
// exact amount; every app-routed succeeded transaction has a
// charge.succeeded or payment.received event).
func (e *env) checkInvariants() {
	e.t.Helper()
	bad, err := store.New(e.db.App).VerifyAuditChain(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if bad != 0 {
		e.t.Fatalf("audit chain broken at %d", bad)
	}
	e.zero("succeeded M-Pesa transaction without a receipt", `
		SELECT count(*) FROM transactions WHERE status = 'succeeded' AND provider = 'mpesa' AND provider_transaction_id IS NULL`)
	e.zero("succeeded charge whose paying transaction is not a succeeded payment for exactly its amount", `
		SELECT count(*) FROM charges c LEFT JOIN transactions t ON t.id = c.transaction_id
		 WHERE c.status IN ('succeeded', 'reversed')
		   AND (t.id IS NULL OR t.status <> 'succeeded' OR t.kind <> 'payment' OR t.amount <> c.amount OR t.charge_id <> c.id)`)
	e.zero("receipt recorded twice", `
		SELECT count(*) FROM (SELECT provider_transaction_id FROM transactions WHERE provider_transaction_id IS NOT NULL
		                       GROUP BY provider, provider_transaction_id HAVING count(*) > 1) x`)
	e.zero("succeeded push whose money is not recorded", `
		SELECT count(*) FROM stk_pushes p WHERE p.status = 'succeeded'
		   AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.status = 'succeeded'
		                    AND (t.id = p.transaction_id OR t.id = p.paid_transaction_id
		                         OR t.provider_request_id = p.checkout_request_id))`)
	e.zero("live push whose transaction is not pending", `
		SELECT count(*) FROM stk_pushes p JOIN transactions t ON t.id = p.transaction_id
		 WHERE p.status IN ('initiating', 'sent') AND t.status <> 'pending'`)
	e.zero("events whose webhook job is missing", `
		SELECT count(*) FROM events ev WHERE NOT EXISTS (
		  SELECT 1 FROM river_job j WHERE j.kind = 'webhook_delivery' AND j.args->>'event_id' = ev.id::text)`)
	e.zero("stored callbacks without a processing job", `
		SELECT count(*) FROM mpesa_callbacks c WHERE NOT EXISTS (
		  SELECT 1 FROM river_job j WHERE j.kind = 'mpesa_process_callback' AND j.args->>'callback_id' = c.id::text)`)
}

func (e *env) assertNoLeak() {
	e.t.Helper()
	if strings.Contains(e.logs.String(), "254712345678") {
		e.t.Fatal("logs contain an unmasked phone number")
	}
}

func mustErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func netipAddr() netip.Addr { return netip.MustParseAddr("196.201.214.200") }

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TODO(gateway-core): these helpers serve stk_test.go, which compiles only
// with -tags gatewaycore until it is ported to charges. Referenced here so
// the linter keeps them; delete this block once the port lands.
var _ = []any{
	(*env).advance, (*env).app, (*env).charge, (*fakeDaraja).setQuery, (*fakeDaraja).setPushErr, (*fakeDaraja).lastPush, (*env).chargeState, newReceipt, (*env).send, (*env).push, (*env).txn,
	(*env).deliver, stkSuccess, stkFailure, c2b, (*env).events, (*env).assertNoLeak, mustErr,
}
