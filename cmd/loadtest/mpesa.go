package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// STK phone endings and how often they are used. The fake Daraja decides the
// outcome by the last digit (internal/mpesa/mpesatest): 0,7-9 paid; 1
// cancelled; 2 "paid" without a receipt (needs review); 3 no answer (left
// waiting, so paybill payments can match it); 6 duplicate callbacks.
var stkEndings = []int{0, 0, 0, 0, 7, 8, 9, 1, 1, 2, 3, 3, 6}

// paidEndings are the endings whose prompt the fake Daraja pays with a
// receipt: those charges must end succeeded.
var paidEndings = map[int]bool{0: true, 5: true, 6: true, 7: true, 8: true, 9: true}

// stkPhone is a random Safaricom number ending in d.
func stkPhone(r *mrand.Rand, d int) string { return fmt.Sprintf("07%07d%d", r.IntN(10_000_000), d) }

// liveCharge is a pending charge a paybill payment can pay: its account
// number and exact amount.
type liveCharge struct {
	id      string
	account string
	amount  int64
}

// livePool holds charges that are still waiting for money, so the c2b op
// can pay one at the paybill.
type livePool struct {
	mu sync.Mutex
	p  []liveCharge
}

const livePoolSize = 5000

func (lp *livePool) add(c liveCharge) {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	if len(lp.p) >= livePoolSize {
		lp.p = lp.p[1:]
	}
	lp.p = append(lp.p, c)
}

// take removes and returns a random live charge.
func (lp *livePool) take(r *mrand.Rand) (liveCharge, bool) {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	if len(lp.p) == 0 {
		return liveCharge{}, false
	}
	i := r.IntN(len(lp.p))
	c := lp.p[i]
	lp.p = append(lp.p[:i], lp.p[i+1:]...)
	return c, true
}

// remove drops a charge (e.g. after it was cancelled).
func (lp *livePool) remove(id string) {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	for i, c := range lp.p {
		if c.id == id {
			lp.p = append(lp.p[:i], lp.p[i+1:]...)
			return
		}
	}
}

// stk creates a charge with a phone, so the gateway sends the first prompt
// straight away. The fake Daraja picks the outcome by the last digit.
func (g *generator) stk(ctx context.Context, r *mrand.Rand) error {
	n := g.seq.Add(1)
	end := stkEndings[r.IntN(len(stkEndings))]
	ch, err := g.client.createCharge(ctx, fmt.Sprintf("lts-%s-%d", g.runID, n), ChargeInput{
		Reference: fmt.Sprintf("LTS-%s-%d", g.runID, n), Amount: int64(1 + r.IntN(5000)),
		Description: "Load test prompt", Phone: stkPhone(r, end),
	})
	if err != nil {
		return err
	}
	g.track(ch)
	if p := ch.LatestSTKPush; p != nil && p.Status != "failed" {
		g.stkSent.Add(1)
		if paidEndings[end] {
			g.expect.add(ch.ID, "succeeded")
		}
	}
	if end == 3 {
		g.live.add(liveCharge{id: ch.ID, account: ch.AccountNumber, amount: ch.Amount})
	}
	return nil
}

// c2b injects a paybill payment into the fake Daraja, which posts a real C2B
// confirmation to the api: half pay a live charge of this run (its account
// number and exact amount), a quarter use the app's prefix but match
// nothing (payment.received), a quarter use a prefix no app has
// (unmatched).
func (g *generator) c2b(ctx context.Context, r *mrand.Rand) error {
	if g.fakeDaraja == "" {
		return errors.New("c2b needs -fake-daraja")
	}
	var account string
	var amount int64
	kind := r.IntN(4)
	if kind < 2 {
		if c, ok := g.live.take(r); ok {
			account, amount = c.account, c.amount
			g.expect.add(c.id, "succeeded")
			g.c2bMatching.Add(1)
		} else {
			kind = 2 // nothing live yet
		}
	}
	switch kind {
	case 2:
		// Shorter than any generated account number (always 12), so it can
		// never pay one of them.
		account, amount = fmt.Sprintf("%s-9%04d", g.prefix, r.IntN(10_000)), int64(1+r.IntN(5000))
		g.c2bNoMatch.Add(1)
	case 3:
		account, amount = fmt.Sprintf("%s-1%04d", g.unknownPrefix, r.IntN(10_000)), int64(1+r.IntN(5000))
		g.c2bUnknown.Add(1)
	}
	body, _ := json.Marshal(map[string]any{"bill_ref": account, "amount": amount, "phone": "2547" + fmt.Sprintf("%08d", r.IntN(100_000_000))})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.fakeDaraja+"fake/c2b", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.fakeHTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fake daraja c2b: HTTP %d", resp.StatusCode)
	}
	g.c2bSent.Add(1)
	return nil
}

func newFakeHTTP() *http.Client { return &http.Client{Timeout: 10 * time.Second} }
