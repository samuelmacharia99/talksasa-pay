package db

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/talksasa/talksasa-pay/internal/config"
)

func pgErr(code string) error {
	return fmt.Errorf("db: commit: %w", &pgconn.PgError{Code: code, Message: "x"})
}

func TestClassification(t *testing.T) {
	cases := []struct {
		err                                   error
		retryable, lock, stmt, busy, notFound bool
	}{
		{pgErr("40001"), true, false, false, true, false},
		{pgErr("40P01"), true, false, false, true, false},
		{pgErr("55P03"), false, true, false, true, false},
		{pgErr("57014"), false, false, true, true, false},
		{pgErr("23505"), false, false, false, false, false},
		{errors.New("plain"), false, false, false, false, false},
		{context.DeadlineExceeded, false, false, false, false, false},
		{nil, false, false, false, false, false},
	}
	for _, c := range cases {
		if IsRetryable(c.err) != c.retryable || IsLockTimeout(c.err) != c.lock ||
			IsStatementTimeout(c.err) != c.stmt || IsBusy(c.err) != c.busy {
			t.Errorf("%v: retryable=%v lock=%v stmt=%v busy=%v", c.err,
				IsRetryable(c.err), IsLockTimeout(c.err), IsStatementTimeout(c.err), IsBusy(c.err))
		}
	}
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestRetryTxRetriesTransientThenSucceeds(t *testing.T) {
	before := counterValue(txRetries.WithLabelValues("deadlock_detected"))
	n := 0
	err := retryTx(context.Background(), 3, noSleep, func(context.Context) error {
		n++
		if n < 3 {
			return pgErr("40P01")
		}
		return nil
	})
	if err != nil || n != 3 {
		t.Fatalf("err=%v attempts=%d", err, n)
	}
	if got := counterValue(txRetries.WithLabelValues("deadlock_detected")) - before; got != 2 {
		t.Fatalf("retry metric +%v, want +2", got)
	}
}

func TestRetryTxGivesUpAfterMaxAttempts(t *testing.T) {
	before := counterValue(txRetryExhausted)
	n := 0
	err := retryTx(context.Background(), MaxTxAttempts, noSleep, func(context.Context) error { n++; return pgErr("40001") })
	if !IsRetryable(err) || n != MaxTxAttempts {
		t.Fatalf("err=%v attempts=%d", err, n)
	}
	if counterValue(txRetryExhausted)-before != 1 {
		t.Fatal("exhausted metric not incremented")
	}
}

func TestRetryTxDoesNotRetryOtherErrors(t *testing.T) {
	for _, e := range []error{pgErr("55P03"), pgErr("57014"), pgErr("23505"), errors.New("domain")} {
		n := 0
		err := retryTx(context.Background(), 3, noSleep, func(context.Context) error { n++; return e })
		if !errors.Is(err, e) || n != 1 {
			t.Errorf("%v: attempts=%d err=%v", e, n, err)
		}
	}
}

func TestRetryTxSingleAttemptForNested(t *testing.T) {
	n := 0
	_ = retryTx(context.Background(), 1, noSleep, func(context.Context) error { n++; return pgErr("40001") })
	if n != 1 {
		t.Fatalf("attempts=%d", n)
	}
}

func TestRetryTxStopsWhenContextDone(t *testing.T) {
	// ctx already done after the first failure: no second attempt.
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	err := retryTx(ctx, 3, noSleep, func(context.Context) error { n++; cancel(); return pgErr("40001") })
	if n != 1 || !IsRetryable(err) {
		t.Fatalf("attempts=%d err=%v", n, err)
	}
	// ctx ends during the backoff sleep: returns promptly with the db error.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	n = 0
	start := time.Now()
	err = retryTx(ctx2, 3, func(ctx context.Context, _ time.Duration) error { return sleepCtx(ctx, time.Hour) },
		func(context.Context) error { n++; return pgErr("40P01") })
	if n != 1 || !IsRetryable(err) || time.Since(start) > 2*time.Second {
		t.Fatalf("attempts=%d err=%v took=%v", n, err, time.Since(start))
	}
}

func TestBackoffBounds(t *testing.T) {
	for attempt := 1; attempt < 70; attempt++ {
		for i := 0; i < 50; i++ {
			d := backoff(attempt)
			if d < 0 || d > retryMaxDelay {
				t.Fatalf("attempt %d: %v", attempt, d)
			}
		}
	}
}

func TestSleepCtx(t *testing.T) {
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestApplySessionParams(t *testing.T) {
	rp := map[string]string{}
	applySessionParams(rp, config.DatabaseConfig{StatementTimeout: 15 * time.Second, LockTimeout: 750 * time.Millisecond, IdleInTxTimeout: 30 * time.Second}, "talksasa-pay-api")
	want := map[string]string{"application_name": "talksasa-pay-api", "statement_timeout": "15000", "lock_timeout": "750",
		"idle_in_transaction_session_timeout": "30000", "timezone": "UTC"}
	for k, v := range want {
		if rp[k] != v {
			t.Errorf("%s=%q want %q", k, rp[k], v)
		}
	}
	rp = map[string]string{}
	applySessionParams(rp, config.DatabaseConfig{}, "x")
	if rp["lock_timeout"] != "5000" || rp["idle_in_transaction_session_timeout"] != "60000" {
		t.Errorf("defaults not applied: %v", rp)
	}
	if _, ok := rp["statement_timeout"]; ok {
		t.Error("zero statement timeout should leave the server default")
	}
}

func TestWithTxRequiresActor(t *testing.T) {
	if err := WithTx(context.Background(), nil, "", nil); err == nil {
		t.Fatal("want error")
	}
}

func TestRegisterMetricsIdempotent(t *testing.T) {
	reg := prometheus.NewRegistry()
	if err := RegisterMetrics(reg); err != nil {
		t.Fatal(err)
	}
	if err := RegisterMetrics(reg); err != nil {
		t.Fatal(err)
	}
}

func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		panic(err)
	}
	return m.GetCounter().GetValue()
}
