package integrationtest

// Shared machinery for the M-Pesa chaos suite (mpesa_*_test.go): the STK
// service on a concurrency env, a controllable in-process Daraja client,
// callback bodies, River-like callback processing with retries, and
// mpesaInvariants, which extends checkInvariants with the STK rules:
//
//   - every receipt is recorded at most once; no succeeded M-Pesa
//     transaction lacks a receipt;
//   - every push is in a state consistent with its transaction
//     (initiating/sent <-> pending, unknown <-> needs_review, failed <->
//     failed, succeeded <-> succeeded, or failed + a rescue transaction when
//     a receipt arrived after we gave up);
//   - every pending/needs_review STK transaction belongs to a push; every
//     STK transaction aimed at a request is a push's own or a rescue;
//   - every stored callback is processed (and has its job);
//   - events exactly once per state change: per push at most one of each
//     stk_push.* type, exactly one for its current final state, and one
//     payment_request.paid per paid request (zero otherwise).

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
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/payer"
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
		Pool: m.db.App, Client: client, Ledger: m.ledger, Alloc: m.alloc, Payers: m.payers, Events: m.events,
		Callbacks: queue.NewEnqueuer(m.river), Logger: m.logger, Now: m.now, ShortCode: mpesaShortCode,
	}
	if tune != nil {
		tune(&o)
	}
	m.svc = stk.NewService(o)
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

// sendProduct sends a /v1 push with a fresh idempotency key.
func (m *mpesaEnv) sendProduct(productID, requestID uuid.UUID, ph string) (stk.Push, error) {
	p, _, err := m.svc.Send(m.ctx, audit.Product(productID), stk.SendParams{
		ProductID: productID, RequestID: requestID, Phone: ph, Source: stk.SourceProduct, IdempotencyKey: uuid.NewString(),
	})
	return p, err
}

// mustSend sends a push and fails the test on error.
func (m *mpesaEnv) mustSend(productID, requestID uuid.UUID, ph string) stk.Push {
	m.t.Helper()
	p, err := m.sendProduct(productID, requestID, ph)
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

// succeededFor sums the succeeded incoming payments aimed at a request
// (count, amount).
func (m *mpesaEnv) succeededFor(requestID uuid.UUID) (int64, int64) {
	m.t.Helper()
	var n, sum int64
	if err := m.db.Owner.QueryRow(m.ctx, `SELECT count(*), coalesce(sum(amount), 0)::bigint FROM transactions
		WHERE payment_request_id = $1 AND status = 'succeeded' AND kind = 'payment'`, requestID).Scan(&n, &sum); err != nil {
		m.t.Fatal(err)
	}
	return n, sum
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

// mpesaPayer creates a payer.
func (m *mpesaEnv) mpesaPayer(productID uuid.UUID) payer.Payer {
	return m.payer(productID, fmt.Sprintf("mp-%d", seq.Add(1)))
}

// mpesaInvariants runs checkInvariants plus the STK rules (package doc).
func (m *mpesaEnv) mpesaInvariants(opts invariantOpts) {
	m.t.Helper()
	m.baseInvariants(opts)

	m.zero("receipt recorded twice", `
		SELECT count(*) FROM (SELECT 1 FROM transactions WHERE provider_transaction_id IS NOT NULL
		                       GROUP BY provider, provider_transaction_id HAVING count(*) > 1) x`)
	m.zero("succeeded M-Pesa transaction without a receipt", `
		SELECT count(*) FROM transactions WHERE status = 'succeeded' AND provider = 'mpesa' AND provider_transaction_id IS NULL`)

	// rescue(p): the succeeded transaction a late receipt created for a push
	// we had failed (successAfterGiveUp).
	const rescue = `EXISTS (SELECT 1 FROM transactions r WHERE r.id <> p.transaction_id AND r.status = 'succeeded'
	                         AND r.channel = 'mpesa_stk' AND r.payment_request_id = p.payment_request_id AND r.amount = p.amount)`
	m.zero("push state inconsistent with its transaction", `
		SELECT count(*) FROM stk_pushes p JOIN transactions t ON t.id = p.transaction_id
		 WHERE NOT ((p.status IN ('initiating', 'sent') AND t.status = 'pending')
		         OR (p.status = 'unknown' AND t.status = 'needs_review')
		         OR (p.status = 'failed' AND t.status = 'failed')
		         OR (p.status = 'succeeded' AND t.status = 'succeeded')
		         OR (p.status = 'succeeded' AND t.status = 'failed' AND `+rescue+`))`)
	m.zero("succeeded push with a receipt on its own row AND a rescue row (money twice)", `
		SELECT count(*) FROM stk_pushes p JOIN transactions t ON t.id = p.transaction_id
		 WHERE p.status = 'succeeded' AND t.status = 'succeeded' AND EXISTS (
		       SELECT 1 FROM transactions r WHERE r.id <> p.transaction_id AND r.status = 'succeeded'
		          AND r.channel = 'mpesa_stk' AND r.provider_request_id = p.checkout_request_id)`)
	m.zero("pending/needs_review STK transaction without a push", `
		SELECT count(*) FROM transactions t WHERE t.channel = 'mpesa_stk' AND t.status IN ('pending', 'needs_review')
		   AND NOT EXISTS (SELECT 1 FROM stk_pushes p WHERE p.transaction_id = t.id)`)
	m.zero("STK money aimed at a request that is neither a push's own transaction nor a rescue", `
		SELECT count(*) FROM transactions t WHERE t.channel = 'mpesa_stk' AND t.status = 'succeeded' AND t.payment_request_id IS NOT NULL
		   AND NOT EXISTS (SELECT 1 FROM stk_pushes p WHERE p.transaction_id = t.id)
		   AND NOT EXISTS (SELECT 1 FROM stk_pushes p JOIN transactions f ON f.id = p.transaction_id
		                    WHERE p.status = 'succeeded' AND f.status = 'failed'
		                      AND p.payment_request_id = t.payment_request_id AND p.amount = t.amount)
		   AND NOT EXISTS (SELECT 1 FROM stk_pushes p JOIN transactions f ON f.id = p.transaction_id
		                    WHERE p.status = 'succeeded' AND f.status = 'succeeded' AND f.raw_payload ? 'TransID'
		                      AND p.payment_request_id = t.payment_request_id AND p.amount = t.amount)`) // second payment after a paybill payment completed the prompt
	m.zero("rescued push with more than one rescue transaction", `
		SELECT count(*) FROM stk_pushes p JOIN transactions f ON f.id = p.transaction_id
		 WHERE p.status = 'succeeded' AND f.status = 'failed'
		   AND (SELECT count(*) FROM transactions r WHERE r.id <> p.transaction_id AND r.status = 'succeeded' AND r.channel = 'mpesa_stk'
		         AND (r.provider_request_id = p.checkout_request_id
		              OR EXISTS (SELECT 1 FROM mpesa_callbacks c WHERE c.transaction_id = r.id
		                          AND c.raw->'Body'->'stkCallback'->>'CheckoutRequestID' = p.checkout_request_id))) > 1`)
	m.zero("unprocessed callbacks", `SELECT count(*) FROM mpesa_callbacks WHERE processed_at IS NULL`)
	m.zero("stored callbacks without a processing job", `
		SELECT count(*) FROM mpesa_callbacks c WHERE NOT EXISTS (
		  SELECT 1 FROM river_job j WHERE j.kind = 'mpesa_process_callback' AND j.args->>'callback_id' = c.id::text)`)

	// Events: once per state change.
	m.zero("push with a stk_push.* event type emitted more than once", `
		SELECT count(*) FROM (SELECT 1 FROM events WHERE subject_type = 'stk_push' GROUP BY subject_id, type HAVING count(*) > 1) x`)
	m.zero("push whose current state has no event", `
		SELECT count(*) FROM stk_pushes p WHERE p.status <> 'initiating' AND NOT EXISTS (
		  SELECT 1 FROM events ev WHERE ev.subject_id = p.id AND ev.type = 'stk_push.' ||
		         CASE p.status WHEN 'unknown' THEN 'needs_review' ELSE p.status::text END)`)
	m.zero("succeeded event for a push that is not succeeded", `
		SELECT count(*) FROM stk_pushes p WHERE p.status <> 'succeeded'
		   AND EXISTS (SELECT 1 FROM events ev WHERE ev.subject_id = p.id AND ev.type = 'stk_push.succeeded')`)
	m.zero("sent event for a push Daraja never confirmed", `
		SELECT count(*) FROM stk_pushes p WHERE p.checkout_request_id IS NULL
		   AND EXISTS (SELECT 1 FROM events ev WHERE ev.subject_id = p.id AND ev.type = 'stk_push.sent')`)
	m.zero("paid request without exactly one payment_request.paid event (or an unpaid one with one)", `
		SELECT count(*) FROM payment_requests r
		 WHERE (SELECT count(*) FROM events ev WHERE ev.subject_id = r.id AND ev.type = 'payment_request.paid')
		       <> CASE WHEN r.status = 'paid' THEN 1 ELSE 0 END`)
}

// mustInProgress asserts err is an *stk.InProgressError.
func isInProgress(err error) bool {
	var ip *stk.InProgressError
	return errors.As(err, &ip)
}
