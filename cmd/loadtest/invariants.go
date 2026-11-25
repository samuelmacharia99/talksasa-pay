package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// invariantsSQL is the database-level check (also used by scripts/smoke.sh
// and the deploy runbook). Every row it returns is a violation.
//
//go:embed invariants.sql
var invariantsSQL string

// violations collects invariant failures by check name.
type violations struct {
	mu  sync.Mutex
	ids map[string][]string
	ran []string
}

func newViolations() *violations { return &violations{ids: map[string][]string{}} }

func (v *violations) check(name string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !slices.Contains(v.ran, name) {
		v.ran = append(v.ran, name)
	}
}

func (v *violations) add(name, id string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ids[name] = append(v.ids[name], id)
}

func (v *violations) total() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, ids := range v.ids {
		n += len(ids)
	}
	return n
}

func (v *violations) report(w io.Writer) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, name := range v.ran {
		ids := v.ids[name]
		if len(ids) == 0 {
			fmt.Fprintf(w, "  ok   %s\n", name)
			continue
		}
		sort.Strings(ids)
		ex := ids[:min(len(ids), 5)]
		fmt.Fprintf(w, "  FAIL %s: %d (e.g. %s)\n", name, len(ids), strings.Join(ex, ", "))
	}
}

// apiInvariants checks, through /v1 only, the money invariants for what this
// run did (plan "Verification"):
//
//   - charges reached the status they must (prompt paid -> succeeded, paybill
//     payment aimed at a charge -> succeeded, cancelled -> cancelled);
//   - every succeeded charge is paid by a succeeded, receipted payment of
//     that charge for exactly its amount;
//   - each receipt is recorded once; no succeeded M-Pesa payment lacks one;
//   - every succeeded payment of the app has a charge.succeeded or
//     payment.received event; the feed has no event twice.
//
// It first waits up to settle for asynchronous outcomes (callbacks, paybill
// notices) to land.
func apiInvariants(ctx context.Context, c *apiClient, rc *runCharges, feedStart string, runStart time.Time, settle time.Duration) (*violations, error) {
	v := newViolations()
	all, want := rc.snapshot()

	// 1. Wait for the expected outcomes.
	deadline := time.Now().Add(settle)
	var charges map[string]*Charge
	for {
		var err error
		if charges, err = getCharges(ctx, c, all); err != nil {
			return nil, err
		}
		pending := 0
		for id, st := range want {
			if ch := charges[id]; ch == nil || ch.Status != st {
				pending++
			}
		}
		if pending == 0 || time.Now().After(deadline) {
			break
		}
		if err := sleepCtx(ctx, time.Second); err != nil {
			return nil, err
		}
	}
	v.check("charge_outcome")
	for id, st := range want {
		if ch := charges[id]; ch == nil || ch.Status != st {
			got := "missing"
			if ch != nil {
				got = ch.Status
			}
			v.add("charge_outcome", fmt.Sprintf("%s(%s, want %s)", id, got, st))
		}
	}

	// 2. Succeeded charges and their payments.
	v.check("charge_payment")
	for _, id := range all {
		ch := charges[id]
		if ch == nil || (ch.Status != "succeeded" && ch.Status != "reversed") {
			continue
		}
		if ch.TransactionID == nil {
			v.add("charge_payment", id)
			continue
		}
		t, err := c.getTransaction(ctx, *ch.TransactionID)
		if err != nil {
			return nil, fmt.Errorf("transaction %s: %w", *ch.TransactionID, err)
		}
		if t.Status != "succeeded" || t.Kind != "payment" || t.Amount != ch.Amount ||
			t.ChargeID == nil || *t.ChargeID != ch.ID || t.Receipt == nil || *t.Receipt == "" {
			v.add("charge_payment", id)
		}
	}

	// 3. Transactions since the run started.
	var txns []Transaction
	cursor := ""
	for {
		page, err := c.listTransactions(ctx, runStart.Add(-time.Minute), cursor, 200)
		if err != nil {
			return nil, fmt.Errorf("list transactions: %w", err)
		}
		txns = append(txns, page.Data...)
		if page.NextCursor == nil || *page.NextCursor == "" || len(page.Data) == 0 {
			break
		}
		cursor = *page.NextCursor
	}
	v.check("receipt_once")
	v.check("mpesa_receipt")
	receipts := map[string]string{}
	for _, t := range txns {
		if t.Receipt != nil && *t.Receipt != "" {
			if prev, ok := receipts[*t.Receipt]; ok && prev != t.ID {
				v.add("receipt_once", *t.Receipt)
			}
			receipts[*t.Receipt] = t.ID
		}
		if t.Kind == "payment" && t.Status == "succeeded" && strings.HasPrefix(t.Channel, "mpesa") &&
			(t.Receipt == nil || *t.Receipt == "") {
			v.add("mpesa_receipt", t.ID)
		}
	}

	// 4. Events for the money.
	events, err := feedEvents(ctx, c, feedStart)
	if err != nil {
		return nil, fmt.Errorf("read events feed: %w", err)
	}
	v.check("feed_unique")
	v.check("money_event")
	seen := map[string]bool{}
	told := map[string]bool{} // transaction ids an app event reported
	for _, e := range events {
		if seen[e.ID] {
			v.add("feed_unique", e.ID)
		}
		seen[e.ID] = true
		var d struct {
			ID            string  `json:"id"`
			TransactionID *string `json:"transaction_id"`
		}
		_ = json.Unmarshal(e.Data, &d)
		switch e.Type {
		case "charge.succeeded":
			if d.TransactionID != nil {
				told[*d.TransactionID] = true
			}
		case "payment.received":
			told[d.ID] = true
		}
	}
	for _, t := range txns {
		// Only this run's money: transactions are listed from a minute
		// earlier (clock skew), but the feed is read from the run's start.
		if t.Kind == "payment" && t.Status == "succeeded" && !told[t.ID] && !t.CreatedAt.Before(runStart) {
			v.add("money_event", t.ID)
		}
	}
	return v, nil
}

// getCharges reads charges with a few concurrent requests.
func getCharges(ctx context.Context, c *apiClient, ids []string) (map[string]*Charge, error) {
	out := make(map[string]*Charge, len(ids))
	var mu sync.Mutex
	var firstErr error
	ch := make(chan string)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for id := range ch {
				got, err := c.getCharge(ctx, id)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("charge %s: %w", id, err)
				} else if err == nil {
					out[id] = got
				}
				mu.Unlock()
			}
		})
	}
	for _, id := range ids {
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop || ctx.Err() != nil {
			break
		}
		ch <- id
	}
	close(ch)
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return out, firstErr
}

// dbInvariants runs invariants.sql (every row is a violation).
func dbInvariants(ctx context.Context, dbURL string) (*violations, error) {
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", stripPassword(err, dbURL))
	}
	defer func() { _ = conn.Close(context.Background()) }()
	return queryInvariants(ctx, conn)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func queryInvariants(ctx context.Context, q querier) (*violations, error) {
	v := newViolations()
	v.check("database")
	rows, err := q.Query(ctx, invariantsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, id string
		if err := rows.Scan(&name, &id); err != nil {
			return nil, err
		}
		v.check("database: " + name)
		v.add("database: "+name, id)
	}
	return v, rows.Err()
}

// stripPassword keeps a connection error from echoing the URL's password.
func stripPassword(err error, dbURL string) error {
	msg := err.Error()
	if i := strings.Index(dbURL, "://"); i >= 0 {
		if at := strings.Index(dbURL[i+3:], "@"); at >= 0 {
			if colon := strings.Index(dbURL[i+3:i+3+at], ":"); colon >= 0 {
				if pw := dbURL[i+3+colon+1 : i+3+at]; pw != "" {
					msg = strings.ReplaceAll(msg, pw, "***")
				}
			}
		}
	}
	return fmt.Errorf("%s", msg)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
