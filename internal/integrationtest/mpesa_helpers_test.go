package integrationtest

// Shared machinery for the M-Pesa chaos suite (mpesa_*_test.go): the STK
// service on a concurrency env, a controllable in-process Daraja client,
// callback bodies, River-like callback processing with retries, and
// charge helpers. mpesaInvariants (mpesa_invariants_test.go) lists what
// must hold after every scenario.

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/charge"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/stk"
	"github.com/talksasa/talksasa-pay/internal/store"
)

const mpesaShortCode = "174379"

// ctlClient is an in-process mpesa.Client the tests steer. Every STKPush
// is recorded with a fresh CheckoutRequestID BEFORE the hook runs, i.e.
// Daraja has "accepted" it even if the hook then fails or blocks the call.
type ctlClient struct {
	mu      sync.Mutex
	n       int
	pushes  map[string]mpesa.STKPushRequest // by checkout id
	order   []string
	query   map[string]mpesa.STKQueryResult
	queries atomic.Int64
	// hook (optional) runs after the push is recorded; a non-nil error is
	// returned to the caller instead of Daraja's acceptance.
	hook func(ctx context.Context, req mpesa.STKPushRequest, checkout string) error
}

func newCtlClient() *ctlClient {
	return &ctlClient{pushes: map[string]mpesa.STKPushRequest{}, query: map[string]mpesa.STKQueryResult{}}
}

func (c *ctlClient) STKPush(ctx context.Context, req mpesa.STKPushRequest) (mpesa.STKPushResponse, error) {
	c.mu.Lock()
	c.n++
	co := fmt.Sprintf("ws_CO_%s_%d", strings.ReplaceAll(uuid.NewString(), "-", "")[:12], c.n)
	c.pushes[co] = req
	c.order = append(c.order, co)
	hook := c.hook
	n := c.n
	c.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, req, co); err != nil {
			return mpesa.STKPushResponse{}, err
		}
	}
	return mpesa.STKPushResponse{MerchantRequestID: fmt.Sprintf("mr-%d", n), CheckoutRequestID: co, ResponseCode: "0"}, nil
}

func (c *ctlClient) STKQuery(_ context.Context, checkout string) (mpesa.STKQueryResult, error) {
	c.queries.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.query[checkout]; ok {
		return r, nil
	}
	return mpesa.STKQueryResult{State: mpesa.QueryPending}, nil
}

func (*ctlClient) RegisterC2BURLs(context.Context, mpesa.RegisterURLsRequest) error { return nil }

func (c *ctlClient) setQuery(checkout string, r mpesa.STKQueryResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.query[checkout] = r
}

func (c *ctlClient) setHook(h func(ctx context.Context, req mpesa.STKPushRequest, checkout string) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hook = h
}

func (c *ctlClient) pushCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *ctlClient) last() (string, mpesa.STKPushRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	co := c.order[len(c.order)-1]
	return co, c.pushes[co]
}

// transientErr is what the real client returns on a 5xx / timeout: Daraja
// may or may not have sent the prompt.
var transientErr = &mpesa.Error{Kind: mpesa.Transient, HTTPStatus: 503, Message: "simulated timeout"}

// mpesaEnv is a concurrency env plus the STK service.
type mpesaEnv struct {
	*env
	client  *ctlClient
	svc     *stk.Service
	charges *charge.Service
	offset  atomic.Int64 // service clock offset (ns)
	retries atomic.Int64 // callback processing attempts that failed and were retried
}

// newMpesaEnv builds the STK service on a concurrency env. client nil uses
// a fresh ctlClient; tune adjusts the options.
func newMpesaEnv(t *testing.T, client mpesa.Client, tune func(*stk.Options)) *mpesaEnv {
	t.Helper()
	m := &mpesaEnv{env: newConcEnv(t, concPoolSize)}
	if client == nil {
		m.client = newCtlClient()
		client = m.client
	}
	o := stk.Options{
		Pool: m.db.App, Client: client, Ledger: m.ledger, Events: m.events,
		Callbacks: queue.NewEnqueuer(m.river), Logger: m.logger, Now: m.now, ShortCode: mpesaShortCode,
	}
	if tune != nil {
		tune(&o)
	}
	m.svc = stk.NewService(o)
	m.charges = charge.NewService(charge.Options{Pool: m.db.App, Events: m.events, STK: m.svc, Logger: m.logger, Now: m.now})
	return m
}

func (m *mpesaEnv) now() time.Time { return time.Now().Add(time.Duration(m.offset.Load())) }

// advance moves the service clock forward.
func (m *mpesaEnv) advance(d time.Duration) { m.offset.Add(int64(d)) }

var mpesaPhoneSeq atomic.Int64

// newPhone returns a unique normalised Safaricom number ending in digit
// last (0-9), so orphan matching by phone never crosses tests or payers.
func newPhone(last int) string {
	n := mpesaPhoneSeq.Add(1) % 10_000_000
	return fmt.Sprintf("2547%07d%d", n, last)
}

// sendApp sends a /v1 push with a fresh idempotency key.
func (m *mpesaEnv) sendApp(appID, chargeID uuid.UUID, ph string) (stk.Push, error) {
	p, _, err := m.svc.Send(m.ctx, audit.App(appID), stk.SendParams{
		AppID: appID, ChargeID: chargeID, Phone: ph, Source: stk.SourceApp, IdempotencyKey: uuid.NewString(),
	})
	return p, err
}

// mustSend sends a push and fails the test on error.
func (m *mpesaEnv) mustSend(appID, chargeID uuid.UUID, ph string) stk.Push {
	m.t.Helper()
	p, err := m.sendApp(appID, chargeID, ph)
	if err != nil {
		m.t.Fatalf("send: %v", err)
	}
	return p
}

// store saves a callback like the /cb handler does.
func (m *mpesaEnv) store(kind stk.CallbackKind, raw string) (uuid.UUID, error) {
	return m.svc.StoreCallback(m.ctx, kind, netip.MustParseAddr("196.201.214.200"), []byte(raw))
}

// process runs a callback job the way River would: on error it is retried
// (with a short backoff), up to 30 attempts.
func (m *mpesaEnv) process(ctx context.Context, id uuid.UUID) error {
	var err error
	for attempt := 0; attempt < 30; attempt++ {
		if err = m.svc.ProcessCallback(ctx, id); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
		m.retries.Add(1)
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}
	return fmt.Errorf("callback %s still failing after retries: %w", id, err)
}

// deliver stores and processes a callback (goroutine-safe, no t.Fatal).
func (m *mpesaEnv) deliver(kind stk.CallbackKind, raw string) (uuid.UUID, error) {
	id, err := m.store(kind, raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("store callback: %w", err)
	}
	return id, m.process(m.ctx, id)
}

func (m *mpesaEnv) mustDeliver(kind stk.CallbackKind, raw string) uuid.UUID {
	m.t.Helper()
	id, err := m.deliver(kind, raw)
	if err != nil {
		m.t.Fatal(err)
	}
	return id
}

func (m *mpesaEnv) push(id uuid.UUID) stk.Push {
	m.t.Helper()
	p, err := store.New(m.db.App).GetSTKPushInternal(m.ctx, id)
	if err != nil {
		m.t.Fatal(err)
	}
	return p
}

func (m *mpesaEnv) txn(id uuid.UUID) store.Transaction {
	m.t.Helper()
	t, err := store.New(m.db.App).GetTransactionInternal(m.ctx, id)
	if err != nil {
		m.t.Fatal(err)
	}
	return t
}

// eventsFor counts events of a type about a subject.
func (m *mpesaEnv) eventsFor(subject uuid.UUID, typ string) int64 {
	return m.count(`SELECT count(*) FROM events WHERE subject_id = $1 AND type = $2`, subject, typ)
}

// succeededFor sums the succeeded incoming payments naming a charge
// (count, amount): the one that paid it plus any second payments.
func (m *mpesaEnv) succeededFor(chargeID uuid.UUID) (int64, int64) {
	m.t.Helper()
	var n, sum int64
	if err := m.db.Owner.QueryRow(m.ctx, `SELECT count(*), coalesce(sum(amount), 0)::bigint FROM transactions
		WHERE charge_id = $1 AND status = 'succeeded' AND kind = 'payment'`, chargeID).Scan(&n, &sum); err != nil {
		m.t.Fatal(err)
	}
	return n, sum
}

// chargeOf reloads a charge.
func (m *mpesaEnv) chargeOf(id uuid.UUID) store.Charge {
	m.t.Helper()
	c, err := store.New(m.db.App).GetChargeInternal(m.ctx, id)
	if err != nil {
		m.t.Fatal(err)
	}
	return c
}

// assertCharge fails unless the charge is in status want; a succeeded or
// reversed charge must also be paid by exactly one succeeded payment of
// its amount (the one it names).
func (m *mpesaEnv) assertCharge(id uuid.UUID, want store.ChargeStatus) store.Charge {
	m.t.Helper()
	c := m.chargeOf(id)
	if c.Status != want {
		m.t.Fatalf("charge %s is %s, want %s", id, c.Status, want)
	}
	if want == store.ChargeStatusSucceeded || want == store.ChargeStatusReversed {
		t := m.txn(*c.TransactionID)
		if t.Status != store.TxnStatusSucceeded || t.Amount != c.Amount || *t.ChargeID != c.ID {
			m.t.Fatalf("charge %s paid by %+v", id, t)
		}
	}
	return c
}

// receiptRows counts transactions carrying a receipt.
func (m *mpesaEnv) receiptRows(receipt string) int64 {
	return m.count(`SELECT count(*) FROM transactions WHERE provider = 'mpesa' AND provider_transaction_id = $1`, receipt)
}

// alarms counts alarm log lines containing substr.
func (m *mpesaEnv) alarms(substr string) int {
	n := 0
	for _, line := range strings.Split(m.logs.String(), "\n") {
		if strings.Contains(line, `"alarm":true`) && strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

var mpesaReceiptSeq atomic.Int64

// newMpesaReceipt is a unique receipt in M-Pesa's shape.
func newMpesaReceipt() string { return fmt.Sprintf("TMP%07d", mpesaReceiptSeq.Add(1)) }

func stkSuccessBody(checkout, receipt string, amount int64, ph string) string {
	return fmt.Sprintf(`{"Body":{"stkCallback":{"MerchantRequestID":"mr-x","CheckoutRequestID":%q,"ResultCode":0,
"ResultDesc":"The service request is processed successfully.","CallbackMetadata":{"Item":[
{"Name":"Amount","Value":%d},{"Name":"MpesaReceiptNumber","Value":%q},{"Name":"Balance"},
{"Name":"TransactionDate","Value":20261124101010},{"Name":"PhoneNumber","Value":%s}]}}}}`, checkout, amount, receipt, ph)
}

func stkFailureBody(checkout, code, desc string) string {
	return fmt.Sprintf(`{"Body":{"stkCallback":{"MerchantRequestID":"mr-x","CheckoutRequestID":%q,"ResultCode":%s,"ResultDesc":%q}}}`, checkout, code, desc)
}

func c2bBody(receipt string, amount int64, billRef string) string {
	return fmt.Sprintf(`{"TransactionType":"Pay Bill","TransID":%q,"TransTime":"20261124101010","TransAmount":"%d.00",
"BusinessShortCode":%q,"BillRefNumber":%q,"InvoiceNumber":"","OrgAccountBalance":"","ThirdPartyTransID":"",
"MSISDN":"2547 ***** 678","FirstName":"JOHN"}`, receipt, amount, mpesaShortCode, billRef)
}

// staff is a finance/admin user resolving needs_review payments.
func mpesaStaff() audit.Actor { return audit.Admin(uuid.New()) }

// mustInProgress asserts err is an *stk.InProgressError.
func isInProgress(err error) bool {
	var ip *stk.InProgressError
	return errors.As(err, &ip)
}
