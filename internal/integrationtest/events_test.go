package integrationtest

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// feed returns every event visible in the gap-free feed for a product.
func (e *env) feed(productID uuid.UUID) []store.Event {
	e.t.Helper()
	var out []store.Event
	afterTxid, afterID := int64(0), uuid.Nil
	for {
		page, err := store.New(e.db.App).ListEventsFeed(e.ctx, store.ListEventsFeedParams{
			ProductID: productID, AfterTxid: afterTxid, AfterID: afterID, Lim: 2,
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

// Every money change emits its events in the same transaction, in order,
// with a webhook delivery row and exactly one River job per event.
func TestEventsOutbox(t *testing.T) {
	e := newEnv(t)
	p := e.product("EVT")
	pr := e.payer(p.Product.ID, "c-1")
	r := e.request(p.Product.ID, pr.ID, 1000)

	e.pay(pr, 400, &r.ID, "EVTRCPT0001") // partially paid
	e.pay(pr, 900, &r.ID, "EVTRCPT0002") // paid + 300 credit
	r2 := e.request(p.Product.ID, pr.ID, 1000)
	if _, err := e.requests.Cancel(e.ctx, operator, p.Product.ID, r2.ID); err == nil {
		t.Fatal("cancel of a credit-paid request should fail")
	}
	r3 := e.request(p.Product.ID, pr.ID, 5000) // 0 credit left: stays open
	c1, err := e.requests.Cancel(e.ctx, operator, p.Product.ID, r3.ID)
	if err != nil || c1.AlreadyCancelled {
		t.Fatalf("cancel: %v %+v", err, c1)
	}
	c2, err := e.requests.Cancel(e.ctx, operator, p.Product.ID, r3.ID) // idempotent retry
	if err != nil || !c2.AlreadyCancelled {
		t.Fatalf("second cancel should be an idempotent no-op: %v %+v", err, c2)
	}

	got := types(e.feed(p.Product.ID))
	want := []string{
		events.PaymentRequestCreated,
		events.TransactionAllocated, events.PaymentRequestPartiallyPaid,
		events.TransactionAllocated, events.PaymentRequestPaid, events.PayerCreditChanged,
		events.PaymentRequestCreated, events.PaymentRequestPartiallyPaid, events.PayerCreditChanged, // credit 300 applied to r2
		events.PaymentRequestCreated, events.PaymentRequestCancelled, // r3; second cancel emits nothing
	}
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
	if err := json.Unmarshal(e.feed(p.Product.ID)[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"hosted_token", "idempotency_key", "product_id", "payer_account_id"} {
		if _, ok := data[banned]; ok {
			t.Errorf("event data exposes %s", banned)
		}
	}
	if data["object"] != "payment_request" || data["pay_url"] == "" {
		t.Errorf("unexpected data %v", data)
	}

	var deliveries, jobs int
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT count(*) FROM webhook_deliveries WHERE product_id = $1 AND status = 'pending'`, p.Product.ID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT count(*) FROM river_job WHERE kind = 'webhook_delivery' AND args->>'product_id' = $1`, p.Product.ID.String()).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if deliveries != len(want) || jobs != len(want) {
		t.Fatalf("deliveries=%d jobs=%d, want %d each", deliveries, jobs, len(want))
	}

	// Other products see nothing.
	other := e.product("OTH")
	if n := len(e.feed(other.Product.ID)); n != 0 {
		t.Fatalf("other product sees %d events", n)
	}
	e.assertNoDrift()
}

// A rolled-back money change leaves no event and no job behind.
func TestEventsRollBackWithChange(t *testing.T) {
	e := newEnv(t)
	p := e.product("RBK")
	pr := e.payer(p.Product.ID, "c-1")
	r := e.request(p.Product.ID, pr.ID, 1000)
	before := len(e.feed(p.Product.ID))
	// Duplicate live reference: the insert fails inside the transaction.
	if _, err := e.requests.Create(e.ctx, operator, p.Product.ID, paymentrequest.CreateParams{
		PayerAccountID: pr.ID, ProductReference: r.ProductReference, Description: "dup", Amount: 500, IdempotencyKey: "other-key",
	}); err == nil {
		t.Fatal("duplicate reference accepted")
	}
	if after := len(e.feed(p.Product.ID)); after != before {
		t.Fatalf("failed create left %d events", after-before)
	}
}
