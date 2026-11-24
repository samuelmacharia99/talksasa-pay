package integrationtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// 10. A consumer pages the events feed continuously while 20 writers create
// requests and payments, some of them holding their transaction open (begin,
// emit, sleep, commit) and some rolling back. The feed must never return an
// event twice, never go backwards, and — after a final drain from the
// reader's own cursor — must have returned every committed event and nothing
// from a rolled-back transaction. One River webhook job per event.
func TestConcurrencyEventsFeedGapFree(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("FEED").Product
	const writers = 20
	iters := 12
	if testing.Short() {
		iters = 5
	}
	payers := make([]payer.Payer, writers)
	for w := range payers {
		payers[w] = e.payer(p.ID, fmt.Sprintf("c%d", w))
	}

	var (
		mu         sync.Mutex
		committed  = map[uuid.UUID]bool{} // held-open events that committed
		rolledBack = map[uuid.UUID]bool{}
	)
	heldOpen := func(w, k int) error {
		var id uuid.UUID
		rollback := (w+k)%3 == 0 // a third of held-open transactions roll back
		err := db.WithTx(e.ctx, e.db.App, worker.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
			var err error
			id, err = e.events.Emit(ctx, q, p.ID, "test.held_open", "payer", payers[w].ID, map[string]any{"object": "test", "w": w, "k": k})
			if err != nil {
				return err
			}
			time.Sleep(time.Duration(20+(w*7+k*13)%100) * time.Millisecond)
			if rollback {
				return errRollback
			}
			return nil
		})
		mu.Lock()
		defer mu.Unlock()
		switch {
		case rollback && errors.Is(err, errRollback):
			rolledBack[id] = true
			return nil
		case err == nil:
			committed[id] = true
			return nil
		default:
			return err
		}
	}

	var stop atomic.Bool
	var seen []store.Event
	var curTxid int64
	var curID uuid.UUID
	pages := 0
	readerErr := make(chan error, 1)
	go func() {
		q := store.New(e.db.App)
		seenIDs := map[uuid.UUID]bool{}
		for !stop.Load() {
			page, err := q.ListEventsFeed(e.ctx, store.ListEventsFeedParams{ProductID: p.ID, AfterTxid: curTxid, AfterID: curID, Lim: 7})
			if err != nil {
				readerErr <- err
				return
			}
			pages++
			for _, ev := range page {
				if seenIDs[ev.ID] {
					readerErr <- fmt.Errorf("event %s returned twice", ev.ID)
					return
				}
				if !feedAfter(ev.Txid, ev.ID, curTxid, curID) {
					readerErr <- fmt.Errorf("feed went backwards at %s", ev.ID)
					return
				}
				seenIDs[ev.ID] = true
				seen = append(seen, ev)
				curTxid, curID = ev.Txid, ev.ID
			}
			if len(page) == 0 {
				time.Sleep(2 * time.Millisecond)
			}
		}
		readerErr <- nil
	}()

	errs := storm(writers, func(w int) error {
		pr := payers[w]
		for k := 0; k < iters; k++ {
			switch k % 4 {
			case 0:
				if _, err := e.createReq(p.ID, pr.ID, fmt.Sprintf("W%d-%d", w, k), fmt.Sprintf("w%d-%d", w, k), 500, nil); err != nil {
					return err
				}
			case 1, 3:
				if _, _, err := e.payErr(e.ctx, e.ledger, pr, 300, nil, newReceipt()); err != nil {
					return err
				}
			case 2:
				if err := heldOpen(w, k); err != nil {
					return err
				}
			}
		}
		return nil
	})
	stop.Store(true)
	if err := <-readerErr; err != nil {
		t.Fatal(err)
	}
	requireNoErrors(t, "writer", errs)
	liveSeen := len(seen)

	// Final drain from the reader's own cursor: if the cursor had ever moved
	// past an event that committed later, the drain cannot find it and
	// drainFeed fails ("feed stuck").
	all := e.drainFeed(p.ID, curTxid, curID, seen)
	t.Logf("feed: %d events seen live over %d pages, %d more on final drain; %d held-open committed, %d rolled back",
		liveSeen, pages, len(all)-liveSeen, len(committed), len(rolledBack))

	inFeed := map[uuid.UUID]bool{}
	for _, ev := range all {
		inFeed[ev.ID] = true
	}
	for id := range committed {
		if !inFeed[id] {
			t.Fatalf("committed held-open event %s missing from feed", id)
		}
	}
	for id := range rolledBack {
		if inFeed[id] {
			t.Fatalf("rolled-back event %s appeared in feed", id)
		}
		if c := e.count(`SELECT count(*) FROM river_job WHERE args->>'event_id' = $1`, id.String()); c != 0 {
			t.Fatalf("rolled-back event %s left %d River jobs", id, c)
		}
	}
	if len(rolledBack) == 0 || len(committed) == 0 {
		t.Fatal("scenario did not exercise both commit and rollback of held-open transactions")
	}
	e.checkInvariants(invariantOpts{}) // includes: feed == table, one job per event
}

// 11. Two products hammered at the same time never see each other's rows or
// events, and cross-product attempts made mid-storm are refused.
func TestConcurrencyCrossProductIsolation(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	prods := []uuid.UUID{e.product("ISOA").Product.ID, e.product("ISOB").Product.ID}
	const perProduct = 8
	payers := make([][]payer.Payer, 2)
	for k, pid := range prods {
		for i := 0; i < perProduct; i++ {
			// Same external_ref in both products: must still be distinct payers.
			payers[k] = append(payers[k], e.payer(pid, fmt.Sprintf("shared-%d", i)))
		}
	}

	errs := storm(2*perProduct, func(g int) error {
		k, i := g%2, g/2
		pid, other := prods[k], prods[1-k]
		pr, foreign := payers[k][i], payers[1-k][i]
		for j := 0; j < 6; j++ {
			c, err := e.createReq(pid, pr.ID, fmt.Sprintf("INV-%d-%d", i, j), fmt.Sprintf("k-%d-%d", i, j), 400, nil)
			if err != nil {
				return err
			}
			if _, _, err := e.payErr(e.ctx, e.ledger, pr, 250, &c.Request.ID, newReceipt()); err != nil {
				return err
			}
			// Cross-product attempts, mid-storm.
			if _, err := e.requests.Get(e.ctx, other, c.Request.ID); !errors.Is(err, paymentrequest.ErrNotFound) {
				return fmt.Errorf("other product read request: %w", err)
			}
			if _, err := e.requests.Cancel(e.ctx, audit.Product(other), other, c.Request.ID); !errors.Is(err, paymentrequest.ErrNotFound) {
				return fmt.Errorf("other product cancelled request: %w", err)
			}
			if _, err := e.createReq(other, pr.ID, "X", fmt.Sprintf("x-%d-%d", i, j), 10, nil); !errors.Is(err, paymentrequest.ErrPayerNotFound) {
				return fmt.Errorf("other product billed payer: %w", err)
			}
			// A payment routed to this product but naming the other
			// product's payer is refused by the composite foreign keys.
			if _, _, err := e.ledger.RecordAndAllocate(e.ctx, worker, ledger.IncomingPayment{
				Channel: "mpesa_c2b", Receipt: newReceipt(), Amount: 5, ProductID: &pid, PayerAccountID: &foreign.ID,
			}); err == nil {
				return errors.New("payment crossed products")
			}
		}
		return nil
	})
	requireNoErrors(t, "worker", errs)

	for k, pid := range prods {
		other := prods[1-k]
		for _, ev := range e.drainFeed(pid, 0, uuid.Nil, nil) {
			if ev.ProductID != pid {
				t.Fatalf("product %s feed returned an event of %s", pid, ev.ProductID)
			}
		}
		for _, tbl := range []string{"payment_requests", "transactions", "allocation_batches", "allocations", "events", "webhook_deliveries"} {
			n := e.count(`SELECT count(*) FROM `+tbl+` WHERE product_id = $1`, pid)
			if n == 0 {
				t.Fatalf("%s: no rows for product %s", tbl, pid)
			}
		}
		e.zero("allocations naming the other product's payer", `SELECT count(*) FROM allocations a
			JOIN payer_accounts pa ON pa.id = a.payer_account_id WHERE a.product_id = $1 AND pa.product_id = $2`, pid, other)
		e.zero("requests naming the other product's payer", `SELECT count(*) FROM payment_requests r
			JOIN payer_accounts pa ON pa.id = r.payer_account_id WHERE r.product_id = $1 AND pa.product_id = $2`, pid, other)
		e.zero("events about the other product's subjects", `SELECT count(*) FROM events ev
			WHERE ev.product_id = $1 AND (ev.subject_id IN (SELECT id FROM payment_requests WHERE product_id = $2)
			   OR ev.subject_id IN (SELECT id FROM transactions WHERE product_id = $2)
			   OR ev.subject_id IN (SELECT id FROM payer_accounts WHERE product_id = $2))`, pid, other)
		e.zero("webhook jobs addressed to the wrong product", `SELECT count(*) FROM river_job j JOIN events ev ON ev.id::text = j.args->>'event_id'
			WHERE j.kind = 'webhook_delivery' AND ev.product_id = $1 AND j.args->>'product_id' <> $1::text`, pid)
	}
	// Identical work on both sides: identical totals.
	var a, b int64
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT
		(SELECT sum(amount) FROM allocations WHERE product_id = $1), (SELECT sum(amount) FROM allocations WHERE product_id = $2)`,
		prods[0], prods[1]).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	if a != b || a != perProduct*6*250 {
		t.Fatalf("allocated A=%d B=%d, want %d each", a, b, perProduct*6*250)
	}
	// Unmatched-to-payer rows from the refused payments must not exist.
	e.zero("refused cross-product payments left rows", `SELECT count(*) FROM transactions WHERE amount = 5`)
	e.checkInvariants(invariantOpts{})
}
