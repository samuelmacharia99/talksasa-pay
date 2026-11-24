// Package integrationtest exercises the services against a real PostgreSQL
// database with the production schema, triggers and privileges.
package integrationtest

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

var (
	operator = audit.CLI("integration-test")
	worker   = audit.System("test-worker")
)

type env struct {
	t        *testing.T
	ctx      context.Context
	db       *testdb.DB
	logs     *syncBuffer
	logger   *slog.Logger
	keyring  *crypto.Keyring
	products *product.Service
	payers   *payer.Service
	requests *paymentrequest.Service
	ledger   *ledger.Service
	alloc    *allocation.Service
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
	alloc := allocation.NewService(logger)
	return &env{
		t: t, ctx: context.Background(), db: d, logs: logs, logger: logger, keyring: kr,
		products: product.NewService(product.Options{
			Pool: d.App, Keyring: kr, APIKeyPepper: []byte(strings.Repeat("p", 40)), AllowHTTPHooks: true, Logger: logger,
		}),
		payers:   payer.NewService(d.App, logger),
		requests: paymentrequest.NewService(d.App, alloc, logger),
		ledger:   ledger.NewService(d.App, alloc, logger),
		alloc:    alloc,
	}
}

func (e *env) product(prefix string) product.Created {
	e.t.Helper()
	c, err := e.products.Create(e.ctx, operator, product.CreateParams{
		Name: prefix + " product", AccountPrefix: prefix, WebhookURL: "https://" + strings.ToLower(prefix) + ".example.com/hooks",
	})
	if err != nil {
		e.t.Fatalf("create product %s: %v", prefix, err)
	}
	return c
}

func (e *env) payer(productID uuid.UUID, ref string) payer.Payer {
	e.t.Helper()
	p, _, err := e.payers.Upsert(e.ctx, operator, productID, payer.UpsertParams{
		ExternalRef: ref, DisplayName: "Customer " + ref, Email: "c" + ref + "@example.com", Phones: []string{"0712345678"},
	})
	if err != nil {
		e.t.Fatalf("create payer: %v", err)
	}
	return p
}

var seq atomic.Int64

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
	// Distinct created_at ordering even on fast machines.
	time.Sleep(2 * time.Millisecond)
	return c.Request
}

var receiptSeq atomic.Int64

func newReceipt() string { return fmt.Sprintf("TST%07d", receiptSeq.Add(1)) }

func (e *env) pay(pr payer.Payer, amount money.KES, target *uuid.UUID, receipt string) (ledger.Transaction, allocation.Result) {
	e.t.Helper()
	ch := store.TxnChannelMpesaC2b
	if target != nil {
		ch = store.TxnChannelMpesaStk
	}
	txn, res, err := e.ledger.RecordAndAllocate(e.ctx, worker, ledger.IncomingPayment{
		Channel: ch, Receipt: receipt, Amount: amount, Phone: "254712345678", BillRef: pr.AccountNumber,
		ProductID: &pr.ProductID, PayerAccountID: &pr.ID, PaymentRequestID: target,
		RawPayload: []byte(`{"TransID":"` + receipt + `"}`),
	})
	if err != nil {
		e.t.Fatalf("pay %d: %v", amount, err)
	}
	return txn, res
}

func (e *env) reqState(productID, id uuid.UUID) paymentrequest.Request {
	e.t.Helper()
	r, err := e.requests.Get(e.ctx, productID, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) credit(productID, payerID uuid.UUID) money.KES {
	e.t.Helper()
	p, err := e.payers.Get(e.ctx, productID, payerID)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.CreditBalance
}

func (e *env) assertNoDrift() {
	e.t.Helper()
	drift, err := store.New(e.db.App).FindBalanceDrift(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if len(drift) != 0 {
		e.t.Fatalf("balance drift: %+v", drift)
	}
	bad, err := store.New(e.db.App).VerifyAuditChain(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if bad != 0 {
		e.t.Fatalf("audit chain broken at %d", bad)
	}
}

func assertStatus(t *testing.T, r paymentrequest.Request, st allocation.Status, paid money.KES) {
	t.Helper()
	if r.Status != st || r.AmountPaid != paid {
		t.Fatalf("request %s: status=%s paid=%d, want status=%s paid=%d", r.ProductReference, r.Status, r.AmountPaid, st, paid)
	}
}

// syncBuffer collects log output from concurrent goroutines.
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
