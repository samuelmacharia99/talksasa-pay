package db

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

// openTestPool opens a pool through Open (so the real session settings
// apply) on a fresh throwaway database with a two-row scratch table.
func openTestPool(t *testing.T, cfg config.DatabaseConfig) *Pool {
	t.Helper()
	tdb := testdb.New(t)
	u, err := url.Parse(os.Getenv("TSP_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("invalid TSP_TEST_DATABASE_URL")
	}
	u.Path = "/" + tdb.Name
	cfg.URL = config.NewSecret(u.String())
	if cfg.MaxConns == 0 {
		cfg.MaxConns = 10
	}
	if cfg.MaxConnLifetime == 0 {
		cfg.MaxConnLifetime = time.Hour
	}
	ctx := context.Background()
	pool, err := Open(ctx, cfg, "tsp-db-test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `CREATE TABLE contention (id int PRIMARY KEY, v int NOT NULL);
		INSERT INTO contention VALUES (1, 0), (2, 0)`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestOpenAppliesSessionSettings(t *testing.T) {
	pool := openTestPool(t, config.DatabaseConfig{StatementTimeout: 7 * time.Second, LockTimeout: 1500 * time.Millisecond, IdleInTxTimeout: 45 * time.Second})
	want := map[string]string{
		"statement_timeout":                   "7s",
		"lock_timeout":                        "1500ms",
		"idle_in_transaction_session_timeout": "45s",
		"application_name":                    "tsp-db-test",
		"TimeZone":                            "UTC",
	}
	// Check several connections, not just the first one.
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := pool.Acquire(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Release()
			for name, v := range want {
				var got string
				if err := conn.QueryRow(context.Background(), "SELECT current_setting($1)", name).Scan(&got); err != nil {
					t.Error(err)
					return
				}
				if got != v {
					t.Errorf("%s = %q, want %q", name, got, v)
				}
			}
			time.Sleep(50 * time.Millisecond) // hold so other goroutines get other conns
		}()
	}
	wg.Wait()
}

func TestWithTxDeadlockVictimIsRetriedAndBothCommit(t *testing.T) {
	pool := openTestPool(t, config.DatabaseConfig{StatementTimeout: 30 * time.Second, LockTimeout: 10 * time.Second})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before := counterValue(txRetries.WithLabelValues("deadlock_detected"))

	var attempts atomic.Int32
	var bothHoldFirst sync.WaitGroup
	bothHoldFirst.Add(2)
	run := func(first, second int) error {
		n := 0
		return WithTx(ctx, pool, "test:deadlock", func(ctx context.Context, _ *store.Queries, tx pgx.Tx) error {
			attempts.Add(1)
			n++
			if _, err := tx.Exec(ctx, "UPDATE contention SET v = v + 1 WHERE id = $1", first); err != nil {
				return err
			}
			if n == 1 { // first attempt: make sure both hold one lock -> guaranteed deadlock
				bothHoldFirst.Done()
				bothHoldFirst.Wait()
			}
			_, err := tx.Exec(ctx, "UPDATE contention SET v = v + 1 WHERE id = $1", second)
			return err
		})
	}
	errs := make(chan error, 2)
	go func() { errs <- run(1, 2) }()
	go func() { errs <- run(2, 1) }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("transaction failed despite retry: %v", err)
		}
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3 (one deadlock victim retried once)", got)
	}
	var v1, v2 int
	if err := pool.QueryRow(ctx, "SELECT (SELECT v FROM contention WHERE id=1), (SELECT v FROM contention WHERE id=2)").Scan(&v1, &v2); err != nil {
		t.Fatal(err)
	}
	if v1 != 2 || v2 != 2 {
		t.Fatalf("rows = %d,%d; want 2,2 (each tx applied exactly once)", v1, v2)
	}
	if counterValue(txRetries.WithLabelValues("deadlock_detected"))-before < 1 {
		t.Fatal("deadlock retry not counted")
	}
}

func TestWithTxSerializationFailureIsRetried(t *testing.T) {
	pool := openTestPool(t, config.DatabaseConfig{StatementTimeout: 10 * time.Second})
	ctx := context.Background()
	n := 0
	err := WithTx(ctx, pool, "test:serialization", func(ctx context.Context, _ *store.Queries, tx pgx.Tx) error {
		n++
		if _, err := tx.Exec(ctx, "UPDATE contention SET v = v + 10 WHERE id = 1"); err != nil {
			return err
		}
		if n == 1 { // server-raised 40001, as a concurrent SERIALIZABLE tx would cause
			_, err := tx.Exec(ctx, `DO $$ BEGIN RAISE EXCEPTION 'could not serialize' USING ERRCODE = 'serialization_failure'; END $$`)
			return err
		}
		return nil
	})
	if err != nil || n != 2 {
		t.Fatalf("err=%v attempts=%d", err, n)
	}
	var v int
	if err := pool.QueryRow(ctx, "SELECT v FROM contention WHERE id = 1").Scan(&v); err != nil || v != 10 {
		t.Fatalf("v=%d err=%v; the failed attempt must have been rolled back", v, err)
	}
}

func TestWithTxRealSerializationConflictIsRetried(t *testing.T) {
	pool := openTestPool(t, config.DatabaseConfig{StatementTimeout: 10 * time.Second})
	ctx := context.Background()
	n := 0
	err := WithTx(ctx, pool, "test:rr", func(ctx context.Context, _ *store.Queries, tx pgx.Tx) error {
		n++
		// On the first attempt fn also drives a REPEATABLE READ transaction
		// whose snapshot is overtaken by a concurrent commit, so the server
		// raises a genuine 40001 that fn returns. WithTx must re-run fn.
		if n == 1 {
			c, err := pool.Acquire(ctx)
			if err != nil {
				return err
			}
			defer c.Release()
			rr, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				return err
			}
			defer func() { _ = rr.Rollback(ctx) }()
			var v int
			if err := rr.QueryRow(ctx, "SELECT v FROM contention WHERE id = 2").Scan(&v); err != nil { // take snapshot
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE contention SET v = v + 1 WHERE id = 2"); err != nil {
				return err
			}
			if _, err := pool.Exec(ctx, "UPDATE contention SET v = v + 100 WHERE id = 1"); err != nil {
				return err
			}
			_, err = rr.Exec(ctx, "UPDATE contention SET v = v + 1 WHERE id = 1")
			return err // 40001: concurrent update since rr's snapshot
		}
		_, err := tx.Exec(ctx, "UPDATE contention SET v = v + 1 WHERE id = 2")
		return err
	})
	if err != nil || n != 2 {
		t.Fatalf("err=%v attempts=%d", err, n)
	}
	var v1, v2 int
	if err := pool.QueryRow(ctx, "SELECT (SELECT v FROM contention WHERE id=1), (SELECT v FROM contention WHERE id=2)").Scan(&v1, &v2); err != nil {
		t.Fatal(err)
	}
	if v1 != 100 || v2 != 1 {
		t.Fatalf("v1=%d v2=%d", v1, v2)
	}
}

func TestWithTxLockTimeoutSurfacesQuicklyAndIsNotRetried(t *testing.T) {
	pool := openTestPool(t, config.DatabaseConfig{StatementTimeout: 10 * time.Second, LockTimeout: 200 * time.Millisecond})
	ctx := context.Background()
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM contention WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	n := 0
	start := time.Now()
	err = WithTx(ctx, pool, "test:locktimeout", func(ctx context.Context, _ *store.Queries, tx pgx.Tx) error {
		n++
		_, err := tx.Exec(ctx, "SELECT 1 FROM contention WHERE id = 1 FOR UPDATE")
		return err
	})
	el := time.Since(start)
	if !IsLockTimeout(err) || !IsBusy(err) || IsRetryable(err) {
		t.Fatalf("err=%v; want lock_not_available", err)
	}
	if n != 1 {
		t.Fatalf("attempts=%d; lock timeouts must not be retried", n)
	}
	if el > 2*time.Second {
		t.Fatalf("lock timeout took %v", el)
	}
}

func TestWithTxStatementTimeoutIsBusy(t *testing.T) {
	pool := openTestPool(t, config.DatabaseConfig{StatementTimeout: 150 * time.Millisecond, LockTimeout: 100 * time.Millisecond})
	n := 0
	err := WithTx(context.Background(), pool, "test:stmt", func(ctx context.Context, _ *store.Queries, tx pgx.Tx) error {
		n++
		_, err := tx.Exec(ctx, "SELECT pg_sleep(2)")
		return err
	})
	if !IsStatementTimeout(err) || !IsBusy(err) || n != 1 {
		t.Fatalf("err=%v attempts=%d", err, n)
	}
}

func TestWithTxNestedSavepointNotRetried(t *testing.T) {
	pool := openTestPool(t, config.DatabaseConfig{StatementTimeout: 10 * time.Second})
	ctx := context.Background()
	outerRuns, innerRuns := 0, 0
	err := WithTx(ctx, pool, "test:outer", func(ctx context.Context, _ *store.Queries, tx pgx.Tx) error {
		outerRuns++
		ierr := WithTx(ctx, tx, "test:inner", func(ctx context.Context, _ *store.Queries, sp pgx.Tx) error {
			innerRuns++
			_, err := sp.Exec(ctx, `DO $$ BEGIN RAISE EXCEPTION USING ERRCODE = 'serialization_failure'; END $$`)
			return err
		})
		if outerRuns == 1 {
			return ierr // the outer transaction owns the retry
		}
		return nil
	})
	if err != nil || outerRuns != 2 || innerRuns != 2 {
		t.Fatalf("err=%v outer=%d inner=%d", err, outerRuns, innerRuns)
	}
}

func TestWithTxDomainErrorRollsBackOnce(t *testing.T) {
	pool := openTestPool(t, config.DatabaseConfig{StatementTimeout: 10 * time.Second})
	ctx := context.Background()
	sentinel := errors.New("domain rule")
	n := 0
	err := WithTx(ctx, pool, "test:domain", func(ctx context.Context, _ *store.Queries, tx pgx.Tx) error {
		n++
		if _, err := tx.Exec(ctx, "UPDATE contention SET v = 99 WHERE id = 1"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) || n != 1 {
		t.Fatalf("err=%v n=%d", err, n)
	}
	var v int
	if err := pool.QueryRow(ctx, "SELECT v FROM contention WHERE id = 1").Scan(&v); err != nil || v != 0 {
		t.Fatalf("v=%d err=%v", v, err)
	}
}

// TestWithTxManyConcurrentWritersAllCommit hammers two hot rows from many
// goroutines in opposite lock orders: every transaction must end up applied
// exactly once (deadlock victims are retried) or fail with a retryable error
// after exhausting attempts — never lose or double-apply an update.
func TestWithTxManyConcurrentWritersAllCommit(t *testing.T) {
	pool := openTestPool(t, config.DatabaseConfig{StatementTimeout: 30 * time.Second, LockTimeout: 10 * time.Second, MaxConns: 20})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const workers = 8
	var committed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			first, second := 1, 2
			if i%2 == 1 {
				first, second = 2, 1
			}
			err := WithTx(ctx, pool, "test:hammer", func(ctx context.Context, _ *store.Queries, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, "UPDATE contention SET v = v + 1 WHERE id = $1", first); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, "UPDATE contention SET v = v + 1 WHERE id = $1", second)
				return err
			})
			switch {
			case err == nil:
				committed.Add(1)
			case IsRetryable(err):
				// exhausted retries under extreme contention: acceptable, but not applied
			default:
				t.Errorf("worker %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	var v1, v2 int
	if err := pool.QueryRow(ctx, "SELECT (SELECT v FROM contention WHERE id=1), (SELECT v FROM contention WHERE id=2)").Scan(&v1, &v2); err != nil {
		t.Fatal(err)
	}
	c := int(committed.Load())
	if v1 != c || v2 != c {
		t.Fatalf("committed=%d but rows=%d,%d", c, v1, v2)
	}
	if c < workers/2 {
		t.Fatalf("only %d/%d committed", c, workers)
	}
}
