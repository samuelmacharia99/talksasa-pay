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

	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// 10. A consumer pages the events feed continuously while 20 writers create
// charges and payments, some of them holding their transaction open (begin,
// emit, sleep, commit) and some rolling back. The feed must never return an
// event twice, never go backwards, and — after a final drain from the
// reader's own cursor — must have returned every committed event and nothing
// from a rolled-back transaction. One River webhook job per event.
func TestConcurrencyEventsFeedGapFree(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.app("FEED").App
	const writers = 20
	iters := 12
	if testing.Short() {
		iters = 5
	}
	subjects := make([]store.Charge, writers)
	for w := range subjects {
		subjects[w] = e.charge(p.ID, 100)
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
			id, err = e.events.Emit(ctx, q, p.ID, "test.held_open", events.SubjectCharge, subjects[w].ID, map[string]any{"object": "test", "w": w, "k": k})
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
			page, err := q.ListEventsFeed(e.ctx, store.ListEventsFeedParams{AppID: p.ID, AfterTxid: curTxid, AfterID: curID, Lim: 7})
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
		for k := 0; k < iters; k++ {
			switch k % 4 {
			case 0:
				c, err := e.insertCharge(e.ctx, p.ID, fmt.Sprintf("W%d-%d", w, k), fmt.Sprintf("w%d-%d", w, k), 500)
				if err != nil {
					return err
				}
				if _, err := e.paymentErr(e.ctx, &p.ID, &c.ID, 500, newReceipt()); err != nil {
					return err
				}
			case 1, 3:
				if _, err := e.paymentErr(e.ctx, &p.ID, nil, 300, newReceipt()); err != nil {
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
	e.checkInvariants() // includes: feed == table, one job per event
}

// 11. Two apps hammered at the same time never see each other's rows or
// events, and cross-app attempts made mid-storm are refused.
func TestConcurrencyCrossAppIsolation(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	apps := []uuid.UUID{e.app("ISOA").App.ID, e.app("ISOB").App.ID}
	const perApp = 8

	errs := storm(2*perApp, func(g int) error {
		k, i := g%2, g/2
		aid, other := apps[k], apps[1-k]
		q := store.New(e.db.App)
		for j := 0; j < 6; j++ {
			// Same reference in both apps: must still be distinct charges.
			c, err := e.insertCharge(e.ctx, aid, fmt.Sprintf("INV-%d-%d", i, j), fmt.Sprintf("k-%d-%d-%d", k, i, j), 250)
			if err != nil {
				return err
			}
			if _, err := e.paymentErr(e.ctx, &aid, &c.ID, 250, newReceipt()); err != nil {
				return err
			}
			// Cross-app attempts, mid-storm.
			if _, err := q.GetChargeForApp(e.ctx, store.GetChargeForAppParams{AppID: other, ID: c.ID}); !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("other app read charge: %w", err)
			}
			if _, err := q.CancelCharge(e.ctx, store.CancelChargeParams{AppID: other, ID: c.ID}); !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("other app cancelled charge: %w", err)
			}
			// A payment routed to the other app but naming this app's
			// charge is refused by the composite foreign key.
			if _, err := e.paymentErr(e.ctx, &other, &c.ID, 5, newReceipt()); err == nil {
				return errors.New("payment crossed apps")
			}
		}
		return nil
	})
	requireNoErrors(t, "worker", errs)

	for k, aid := range apps {
		other := apps[1-k]
		for _, ev := range e.drainFeed(aid, 0, uuid.Nil, nil) {
			if ev.AppID != aid {
				t.Fatalf("app %s feed returned an event of %s", aid, ev.AppID)
			}
		}
		for _, tbl := range []string{"charges", "transactions", "events", "webhook_deliveries"} {
			if n := e.count(`SELECT count(*) FROM `+tbl+` WHERE app_id = $1`, aid); n == 0 {
				t.Fatalf("%s: no rows for app %s", tbl, aid)
			}
		}
		e.zero("transactions naming the other app's charge", `SELECT count(*) FROM transactions t
			JOIN charges c ON c.id = t.charge_id WHERE t.app_id = $1 AND c.app_id = $2`, aid, other)
		e.zero("events about the other app's subjects", `SELECT count(*) FROM events ev
			WHERE ev.app_id = $1 AND (ev.subject_id IN (SELECT id FROM charges WHERE app_id = $2)
			   OR ev.subject_id IN (SELECT id FROM transactions WHERE app_id = $2))`, aid, other)
		e.zero("webhook jobs addressed to the wrong app", `SELECT count(*) FROM river_job j JOIN events ev ON ev.id::text = j.args->>'event_id'
			WHERE j.kind = 'webhook_delivery' AND ev.app_id = $1 AND j.args->>'app_id' <> $1::text`, aid)
	}
	// Identical work on both sides: identical totals.
	var a, b int64
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT
		(SELECT sum(amount) FROM charges WHERE app_id = $1 AND status = 'succeeded'),
		(SELECT sum(amount) FROM charges WHERE app_id = $2 AND status = 'succeeded')`,
		apps[0], apps[1]).Scan(&a, &b); err != nil {
		t.Fatal(err)
	}
	if a != b || a != perApp*6*250 {
		t.Fatalf("succeeded A=%d B=%d, want %d each", a, b, perApp*6*250)
	}
	// The refused cross-app payments must have left no rows.
	e.zero("refused cross-app payments left rows", `SELECT count(*) FROM transactions WHERE amount = 5`)
	e.checkInvariants()
}
