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

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/store"
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
	products *product.Service
	payers   *payer.Service
	requests *paymentrequest.Service
	ledger   *ledger.Service
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
	alloc := allocation.NewService(logger, ev)
	payers := payer.NewService(d.App, logger)
	led := ledger.NewService(d.App, alloc, logger)
	e := &env{
		t: t, ctx: context.Background(), db: d, logs: logs, fake: newFake(),
		products: product.NewService(product.Options{
			Pool: d.App, Keyring: kr, APIKeyPepper: []byte(strings.Repeat("p", 40)), AllowHTTPHooks: true, Logger: logger,
		}),
		payers:   payers,
		requests: paymentrequest.NewService(d.App, alloc, ev, logger),
		ledger:   led,
	}
	e.svc = NewService(Options{
		Pool: d.App, Client: e.fake, Ledger: led, Alloc: alloc, Payers: payers, Events: ev, Callbacks: enq, Logger: logger,
		Now: e.now, ShortCode: testShortCode,
	})
	return e
}

func (e *env) now() time.Time { return time.Now().Add(time.Duration(e.offset.Load())) }

// advance moves the service clock forward.
func (e *env) advance(d time.Duration) { e.offset.Add(int64(d)) }

func (e *env) product(prefix string) uuid.UUID {
	e.t.Helper()
	c, err := e.products.Create(e.ctx, operator, product.CreateParams{
		Name: prefix + " product", AccountPrefix: prefix, WebhookURL: "https://" + strings.ToLower(prefix) + ".example.com/hooks",
	})
	if err != nil {
		e.t.Fatalf("create product %s: %v", prefix, err)
	}
	return c.Product.ID
}

var seq atomic.Int64

func (e *env) payer(productID uuid.UUID) payer.Payer {
	e.t.Helper()
	ref := fmt.Sprintf("cust-%d", seq.Add(1))
	p, _, err := e.payers.Upsert(e.ctx, operator, productID, payer.UpsertParams{
		ExternalRef: ref, DisplayName: "Customer " + ref, Phones: []string{"0712345678"},
	})
	if err != nil {
		e.t.Fatalf("create payer: %v", err)
	}
	return p
}

func (e *env) request(productID, payerID uuid.UUID, amount int64) paymentrequest.Request {
	e.t.Helper()
	n := seq.Add(1)
	c, err := e.requests.Create(e.ctx, audit.Product(productID), productID, paymentrequest.CreateParams{
		PayerAccountID: payerID, ProductReference: fmt.Sprintf("INV-%d", n), Description: "Invoice",
		Amount: amount, IdempotencyKey: fmt.Sprintf("idem-%d", n),
	})
	if err != nil {
		e.t.Fatalf("create request: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	return c.Request
}

func (e *env) req(productID, id uuid.UUID) paymentrequest.Request {
	e.t.Helper()
	r, err := e.requests.Get(e.ctx, productID, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

var receiptSeq atomic.Int64

func newReceipt() string { return fmt.Sprintf("TST%07d", receiptSeq.Add(1)) }

// send sends a product push with a fresh key and expects success.
func (e *env) send(productID, requestID uuid.UUID, ph string) Push {
	e.t.Helper()
	p, replayed, err := e.svc.Send(e.ctx, audit.Product(productID), SendParams{
		ProductID: productID, RequestID: requestID, Phone: ph, Source: SourceProduct, IdempotencyKey: uuid.NewString(),
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

// checkInvariants proves the ledger rules hold after a test.
func (e *env) checkInvariants() {
	e.t.Helper()
	q := store.New(e.db.App)
	drift, err := q.FindBalanceDrift(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(drift) != 0 {
		e.t.Fatalf("balance drift: %+v", drift)
	}
	bad, err := q.VerifyAuditChain(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if bad != 0 {
		e.t.Fatalf("audit chain broken at %d", bad)
	}
	e.zero("succeeded M-Pesa transaction without a receipt", `
		SELECT count(*) FROM transactions WHERE status = 'succeeded' AND provider = 'mpesa' AND provider_transaction_id IS NULL`)
	e.zero("paid/partially paid request without a succeeded transaction allocated to it", `
		SELECT count(*) FROM payment_requests r WHERE r.amount_paid > 0
		   AND NOT EXISTS (SELECT 1 FROM allocations a JOIN allocation_batches b ON b.id = a.batch_id
		                     JOIN transactions t ON t.id = b.transaction_id
		                    WHERE a.payment_request_id = r.id AND t.status = 'succeeded')`)
	e.zero("request amount_paid != sum of its allocations", `
		SELECT count(*) FROM payment_requests r
		 WHERE r.amount_paid <> coalesce((SELECT sum(a.amount) FROM allocations a WHERE a.payment_request_id = r.id), 0)`)
	e.zero("payer: allocations != amount_paid + credit", `
		SELECT count(*) FROM payer_accounts pa
		 WHERE coalesce((SELECT sum(a.amount) FROM allocations a WHERE a.payer_account_id = pa.id), 0)
		    <> coalesce((SELECT sum(r.amount_paid) FROM payment_requests r WHERE r.payer_account_id = pa.id), 0) + pa.credit_balance`)
	e.zero("matched succeeded payment without exactly one payment batch", `
		SELECT count(*) FROM transactions t
		 WHERE t.kind = 'payment' AND t.status = 'succeeded' AND t.payer_account_id IS NOT NULL
		   AND (SELECT count(*) FROM allocation_batches b WHERE b.transaction_id = t.id AND b.kind = 'payment') <> 1`)
	e.zero("allocations from a non-succeeded transaction", `
		SELECT count(*) FROM allocation_batches b JOIN transactions t ON t.id = b.transaction_id WHERE t.status <> 'succeeded'`)
	e.zero("receipt recorded twice", `
		SELECT count(*) FROM (SELECT provider_transaction_id FROM transactions WHERE provider_transaction_id IS NOT NULL
		                       GROUP BY provider, provider_transaction_id HAVING count(*) > 1) x`)
	e.zero("succeeded push whose money is not recorded", `
		SELECT count(*) FROM stk_pushes p WHERE p.status = 'succeeded'
		   AND NOT EXISTS (SELECT 1 FROM transactions t WHERE t.status = 'succeeded' AND t.channel = 'mpesa_stk'
		                    AND (t.id = p.transaction_id OR t.provider_request_id = p.checkout_request_id
		                         OR (t.payment_request_id = p.payment_request_id AND t.amount = p.amount)))`)
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
