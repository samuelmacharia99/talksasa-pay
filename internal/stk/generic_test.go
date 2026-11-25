package stk

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/store"
)

func TestCustomerMessage(t *testing.T) {
	for code, want := range map[string]string{
		"1032": "You cancelled the M-Pesa prompt.",
		"1037": "We couldn't reach your phone. Make sure it's on and try again.",
		"1":    "Your M-Pesa balance wasn't enough.",
		"2001": "The M-Pesa PIN was wrong.",
		"1019": "The prompt expired before it was answered.",
		"9999": "The payment didn't go through.",
		"":     "The payment didn't go through.",
	} {
		if got := CustomerMessage(code); got != want {
			t.Errorf("CustomerMessage(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestStoreCallback(t *testing.T) {
	e := newEnv(t)
	for _, raw := range []string{"", "[]", `"x"`, "{bad", `{"a":"` + strings.Repeat("x", MaxCallbackBytes) + `"}`} {
		if _, err := e.svc.StoreCallback(e.ctx, store.MpesaCallbackKindStk, netipAddr(), []byte(raw)); !errors.Is(err, ErrInvalidCallback) {
			t.Fatalf("StoreCallback(%.20q): %v", raw, err)
		}
	}
	id, err := e.svc.StoreCallback(e.ctx, store.MpesaCallbackKindStk, netipAddr(), []byte(` {"a":1} `))
	if err != nil {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM river_job WHERE kind = 'mpesa_process_callback' AND args->>'callback_id' = $1`, id.String()); n != 1 {
		t.Fatalf("jobs: %d", n)
	}
	if cb := e.callback(id); cb.ProcessedAt != nil {
		t.Fatal("processed early")
	}
	// Unknown callback id: no retry loop.
	if err := e.svc.ProcessCallback(e.ctx, uuid.New()); err != nil {
		t.Fatal(err)
	}
	e.checkInvariants()
}

func TestQueryBackoff(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		if got := QueryBackoff(i + 1); got != w {
			t.Fatalf("QueryBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}
