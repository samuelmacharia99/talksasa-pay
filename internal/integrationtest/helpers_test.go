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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/seed"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

var (
	operator = audit.CLI("integration-test")
	worker   = audit.System("test-worker")
)

type env struct {
	t       *testing.T
	ctx     context.Context
	db      *testdb.DB
	logs    *syncBuffer
	logger  *slog.Logger
	keyring *crypto.Keyring
	apps    *app.Service
	ledger  *ledger.Service
	river   *queue.Client
	events  *events.Emitter
	// prefixes by app id (seeded charges need the app's prefix).
	mu       sync.Mutex
	prefixes map[uuid.UUID]string
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
	ev := events.NewEmitter(apiview.View{PayBaseURL: "https://pay.test/", Paybill: "174379"}, queue.NewEnqueuer(rc))
	return &env{
		t: t, ctx: context.Background(), db: d, logs: logs, logger: logger, keyring: kr,
		apps: app.NewService(app.Options{
			Pool: d.App, Keyring: kr, APIKeyPepper: []byte(strings.Repeat("p", 40)), AllowHTTPHooks: true, Logger: logger,
		}),
		ledger:   ledger.NewService(d.App, logger).WithEvents(ev),
		river:    rc,
		events:   ev,
		prefixes: map[uuid.UUID]string{},
	}
}

func (e *env) app(prefix string) app.Created {
	e.t.Helper()
	c, err := e.apps.Create(e.ctx, operator, app.CreateParams{
		Name: prefix + " app", AccountPrefix: prefix, WebhookURL: "https://" + strings.ToLower(prefix) + ".example.com/hooks",
	})
	if err != nil {
		e.t.Fatalf("create app %s: %v", prefix, err)
	}
	e.mu.Lock()
	e.prefixes[c.App.ID] = prefix
	e.mu.Unlock()
	return c
}

// charge seeds a pending charge (store queries; the charge service is not
// under test here).
func (e *env) charge(appID uuid.UUID, amount int64) store.Charge {
	e.t.Helper()
	e.mu.Lock()
	prefix := e.prefixes[appID]
	e.mu.Unlock()
	return seed.Charge(e.t, e.ctx, e.db.App, appID, prefix, amount, seed.ChargeOpts{})
}

var receiptSeq atomic.Int64

func newReceipt() string { return fmt.Sprintf("TST%07d", receiptSeq.Add(1)) }

// payCharge records a paybill payment that pays c (no events).
func (e *env) payCharge(c store.Charge) store.Transaction {
	e.t.Helper()
	return seed.PayCharge(e.t, e.ctx, e.db.App, c)
}

// emitCharge writes a charge event through the outbox.
func (e *env) emitCharge(appID, chargeID uuid.UUID, typ string) {
	e.t.Helper()
	if err := db.WithTx(e.ctx, e.db.App, worker.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		return e.events.EmitCharge(ctx, q, appID, chargeID, typ)
	}); err != nil {
		e.t.Fatal(err)
	}
}

// assertAuditChain fails the test if the audit hash chain is broken.
func (e *env) assertAuditChain() {
	e.t.Helper()
	bad, err := store.New(e.db.App).VerifyAuditChain(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	if bad != 0 {
		e.t.Fatalf("audit chain broken at %d", bad)
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
