package integrationtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// feed returns every event visible in the gap-free feed for an app.
func (e *env) feed(appID uuid.UUID) []store.Event {
	e.t.Helper()
	// The feed hides events until every older transaction on the SERVER has
	// finished (txids are cluster-wide), so parallel test packages can delay
	// it. Wait until it has caught up with this app's newest event.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var settled bool
		if err := e.db.Owner.QueryRow(e.ctx, `SELECT coalesce(max(txid), 0) < (pg_snapshot_xmin(pg_current_snapshot())::text)::bigint FROM events WHERE app_id = $1`, appID).Scan(&settled); err != nil {
			e.t.Fatal(err)
		}
		if settled {
			break
		}
		if time.Now().After(deadline) {
			e.t.Fatal("events feed did not settle within 30s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var out []store.Event
	afterTxid, afterID := int64(0), uuid.Nil
	for {
		page, err := store.New(e.db.App).ListEventsFeed(e.ctx, store.ListEventsFeedParams{
			AppID: appID, AfterTxid: afterTxid, AfterID: afterID, Lim: 2,
		})
		if err != nil {
			e.t.Fatal(err)
		}
		if len(page) == 0 {
			return out
		}
		out = append(out, page...)
		last := page[len(page)-1]
		afterTxid, afterID = last.Txid, last.ID
	}
}

func types(evs []store.Event) []string {
	out := make([]string, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

// Events go through the outbox in the writing transaction, in order, with a
// webhook delivery row and exactly one River job per event. (Which events a
// money change emits is tested with the money core: TODO(gateway-core).)
func TestEventsOutbox(t *testing.T) {
	e := newEnv(t)
	p := e.app("EVT")
	c1 := e.charge(p.App.ID, 1000)
	c2 := e.charge(p.App.ID, 2000)
	txn := e.payCharge(c1)
	e.emitCharge(p.App.ID, c1.ID, events.ChargeSucceeded)
	e.emitCharge(p.App.ID, c2.ID, events.ChargeExpired)
	if err := db.WithTx(e.ctx, e.db.App, worker.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		return e.events.EmitPaymentReceived(ctx, q, p.App.ID, txn.ID, &c1.ID)
	}); err != nil {
		t.Fatal(err)
	}

	evs := e.feed(p.App.ID)
	got := types(evs)
	want := []string{events.ChargeSucceeded, events.ChargeExpired, events.PaymentReceived}
	if len(got) != len(want) {
		t.Fatalf("events:\n got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d: got %s want %s\nall: %v", i, got[i], want[i], got)
		}
	}

	// Data is the public resource: no internals.
	var data map[string]any
	if err := json.Unmarshal(evs[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"hosted_token", "idempotency_key", "app_id", "request_hash", "account_number_norm"} {
		if _, ok := data[banned]; ok {
			t.Errorf("event data exposes %s", banned)
		}
	}
	if data["object"] != "charge" || data["status"] != "succeeded" || data["transaction_id"] != txn.ID.String() ||
		data["checkout_url"] != "https://pay.test/"+c1.HostedToken {
		t.Errorf("unexpected data %v", data)
	}
	if err := json.Unmarshal(evs[2].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["object"] != "transaction" || data["duplicate_of_charge"] != c1.ID.String() || data["charge_id"] != c1.ID.String() {
		t.Errorf("payment.received data %v", data)
	}

	var deliveries, jobs int
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT count(*) FROM webhook_deliveries WHERE app_id = $1 AND status = 'pending'`, p.App.ID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT count(*) FROM river_job WHERE kind = 'webhook_delivery' AND args->>'app_id' = $1`, p.App.ID.String()).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if deliveries != len(want) || jobs != len(want) {
		t.Fatalf("deliveries=%d jobs=%d, want %d each", deliveries, jobs, len(want))
	}

	// Other apps see nothing.
	other := e.app("OTH")
	if n := len(e.feed(other.App.ID)); n != 0 {
		t.Fatalf("other app sees %d events", n)
	}
	e.assertAuditChain()
}

// A rolled-back change leaves no event and no job behind.
func TestEventsRollBackWithChange(t *testing.T) {
	e := newEnv(t)
	p := e.app("RBK")
	c := e.charge(p.App.ID, 1000)
	before := len(e.feed(p.App.ID))
	err := db.WithTx(e.ctx, e.db.App, worker.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		if err := e.events.EmitCharge(ctx, q, p.App.ID, c.ID, events.ChargeCancelled); err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	if after := len(e.feed(p.App.ID)); after != before {
		t.Fatalf("rolled-back change left %d events", after-before)
	}
	if n := e.count(`SELECT count(*) FROM river_job WHERE kind = 'webhook_delivery' AND args->>'app_id' = $1`, p.App.ID.String()); n != 0 {
		t.Fatalf("rolled-back change left %d jobs", n)
	}
}
