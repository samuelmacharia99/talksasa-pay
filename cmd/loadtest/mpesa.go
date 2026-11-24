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

	"github.com/talksasa/talksasa-pay/pkg/talksasapay"
)

// STK phone endings and how often they are used. The fake Daraja decides the
// outcome by the last digit (internal/mpesa/mpesatest): 0,7-9 paid; 1
// cancelled; 2 "paid" without a receipt (staff review); 3 no answer (left
// waiting, so paybill payments can match it); 6 duplicate callbacks.
var stkEndings = []int{0, 0, 0, 0, 7, 8, 9, 1, 1, 2, 3, 3, 6}

// stkPhone is a random Safaricom number ending in d.
func stkPhone(r *mrand.Rand, d int) string { return fmt.Sprintf("07%07d%d", r.IntN(10_000_000), d) }

// waitingPrompt remembers a prompt that may still be waiting, so a later
// paybill payment can match it (same payer, exact amount).
type waitingPrompt struct {
	payer  int
	amount int64
}

type promptPool struct {
	mu sync.Mutex
	p  []waitingPrompt
}

func (pp *promptPool) add(w waitingPrompt) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if len(pp.p) >= 2000 {
		pp.p = pp.p[1:]
	}
	pp.p = append(pp.p, w)
}

func (pp *promptPool) take(r *mrand.Rand) (waitingPrompt, bool) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	if len(pp.p) == 0 {
		return waitingPrompt{}, false
	}
	i := r.IntN(len(pp.p))
	w := pp.p[i]
	pp.p = append(pp.p[:i], pp.p[i+1:]...)
	return w, true
}

// stk creates a bill and sends a prompt for it.
func (g *generator) stk(ctx context.Context, r *mrand.Rand) error {
	n := g.seq.Add(1)
	payer := r.IntN(g.payers)
	amount := int64(1 + r.IntN(5000))
	pr, _, err := g.client.CreatePaymentRequest(ctx, talksasapay.PaymentRequestInput{
		Payer: g.payerRef(payer), Reference: fmt.Sprintf("LTS-%s-%d", g.runID, n),
		Description: "Load test prompt", Amount: amount, IdempotencyKey: fmt.Sprintf("lts-%s-%d", g.runID, n),
	})
	if err != nil {
		return err
	}
	g.created.add(pr.ID)
	if pr.Outstanding <= 0 {
		return nil // paid from the payer's credit already: nothing to prompt for
	}
	end := stkEndings[r.IntN(len(stkEndings))]
	_, _, err = g.client.STKPush(ctx, pr.ID, stkPhone(r, end), talksasapay.STKPushOptions{
		IdempotencyKey: fmt.Sprintf("lts-%s-%d-push", g.runID, n),
	})
	if err == nil {
		g.stkSent.Add(1)
		if end == 3 {
			g.waiting.add(waitingPrompt{payer: payer, amount: pr.Outstanding})
		}
	}
	return err
}

// c2b injects a paybill payment for one of the run's payers into the fake
// Daraja, which posts a real C2B confirmation to the api. Half of them
// match a waiting prompt (same payer, exact amount) when one is known.
func (g *generator) c2b(ctx context.Context, r *mrand.Rand) error {
	if g.fakeDaraja == "" {
		return errors.New("c2b needs -fake-daraja")
	}
	payer, amount := r.IntN(g.payers), int64(1+r.IntN(5000))
	if r.IntN(2) == 0 {
		if w, ok := g.waiting.take(r); ok {
			payer, amount = w.payer, w.amount
			g.c2bMatching.Add(1)
		}
	}
	acct, ok := g.accounts.Load(payer)
	if !ok {
		return errSkipped
	}
	body, _ := json.Marshal(map[string]any{"bill_ref": acct, "amount": amount, "phone": stkPhone(r, 0)})
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
