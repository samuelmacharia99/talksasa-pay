// Package db owns the PostgreSQL connection pool, the transaction helper and
// error classification. All writes that touch money go through WithTx, which
// runs at READ COMMITTED with explicit row locks (SELECT ... FOR UPDATE) taken
// in a fixed order: stk push -> transaction -> charge.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Pool is the shared connection pool.
type Pool = pgxpool.Pool

// Open creates and verifies a connection pool. Every connection starts with
// these session settings (sent in the startup packet, so they hold before
// the first query and survive DISCARD ALL):
//
//   - statement_timeout = cfg.StatementTimeout: no single statement runs away;
//   - lock_timeout = cfg.LockTimeout: a statement waiting for a row lock fails
//     with lock_not_available (IsLockTimeout) instead of queueing forever;
//   - idle_in_transaction_session_timeout = cfg.IdleInTxTimeout: a leaked
//     open transaction is killed and its locks released;
//   - application_name = appName, timezone = UTC.
func Open(ctx context.Context, cfg config.DatabaseConfig, appName string, logger *slog.Logger) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL.Reveal())
	if err != nil {
		// pgx errors can include the DSN; never wrap them.
		return nil, errors.New("db: invalid database URL")
	}
	pc.MaxConns = cfg.MaxConns
	pc.MinConns = cfg.MinConns
	pc.MaxConnLifetime = cfg.MaxConnLifetime
	pc.MaxConnLifetimeJitter = cfg.MaxConnLifetime / 10
	pc.HealthCheckPeriod = 30 * time.Second
	applySessionParams(pc.ConnConfig.RuntimeParams, cfg, appName)
	pc.ConnConfig.Tracer = &slowQueryTracer{logger: logger, threshold: 250 * time.Millisecond}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", sanitize(err))
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", sanitize(err))
	}
	return pool, nil
}

// Session defaults used when the config leaves a value at zero.
const (
	defaultLockTimeout     = 5 * time.Second
	defaultIdleInTxTimeout = 60 * time.Second
)

func applySessionParams(rp map[string]string, cfg config.DatabaseConfig, appName string) {
	ms := func(d, def time.Duration) string {
		if d <= 0 {
			d = def
		}
		return strconv.FormatInt(d.Milliseconds(), 10)
	}
	rp["application_name"] = appName
	if cfg.StatementTimeout > 0 {
		rp["statement_timeout"] = ms(cfg.StatementTimeout, 0)
	}
	rp["lock_timeout"] = ms(cfg.LockTimeout, defaultLockTimeout)
	rp["idle_in_transaction_session_timeout"] = ms(cfg.IdleInTxTimeout, defaultIdleInTxTimeout)
	rp["timezone"] = "UTC"
}

// TxFunc runs inside a database transaction.
//
// WithTx may call it more than once (see WithTx), each time with a fresh
// transaction, so it must only change state through q/tx: no HTTP calls,
// no sends on channels, no writes to captured variables that a later
// attempt does not overwrite. Results meant for the caller should be
// assigned (not appended) so the last, committed attempt wins.
type TxFunc func(ctx context.Context, q *store.Queries, tx pgx.Tx) error

// Beginner is satisfied by *pgxpool.Pool and pgx.Tx (nested = savepoint).
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// MaxTxAttempts bounds how many times WithTx runs fn when PostgreSQL aborts
// the transaction with serialization_failure or deadlock_detected.
const MaxTxAttempts = 3

// Retry backoff: full jitter over an exponentially growing window.
const (
	retryBaseDelay = 10 * time.Millisecond
	retryMaxDelay  = 250 * time.Millisecond
)

// WithTx runs fn in a READ COMMITTED transaction and commits if fn returns
// nil. actor ("cli:alice", "system:c2b-worker", "app:<id>") is recorded
// by triggers on every ledger status change in this transaction.
//
// If the transaction fails with serialization_failure (40001) or
// deadlock_detected (40P01) — from fn or from COMMIT — the transaction is
// rolled back and the WHOLE of fn is run again in a fresh transaction, up
// to MaxTxAttempts times in total, after a short jittered backoff. No other
// error is retried, and nothing is retried once ctx is done. Savepoints
// (b is itself a pgx.Tx) are never retried: the enclosing transaction owns
// the retry. lock_not_available and statement timeouts are not retried
// either (the lock holder is likely still busy); see IsBusy.
func WithTx(ctx context.Context, b Beginner, actor string, fn TxFunc) error {
	if actor == "" {
		return errors.New("db: actor is required for every write transaction")
	}
	attempts := MaxTxAttempts
	if _, nested := b.(pgx.Tx); nested {
		attempts = 1
	}
	return retryTx(ctx, attempts, sleepCtx, func(ctx context.Context) error {
		return runTx(ctx, b, actor, fn)
	})
}

// retryTx runs once until it succeeds, fails with a non-retryable error,
// runs out of attempts, or ctx ends.
func retryTx(ctx context.Context, attempts int, sleep func(context.Context, time.Duration) error, once func(context.Context) error) error {
	for attempt := 1; ; attempt++ {
		err := once(ctx)
		if err == nil || attempt >= attempts || !IsRetryable(err) || ctx.Err() != nil {
			if err != nil && attempt > 1 && IsRetryable(err) {
				txRetryExhausted.Inc()
			}
			return err
		}
		txRetries.WithLabelValues(retryReason(err)).Inc()
		if serr := sleep(ctx, backoff(attempt)); serr != nil {
			return err // ctx ended while waiting: report the database error
		}
	}
}

// backoff returns a random delay in [0, min(max, base*2^(attempt-1))].
func backoff(attempt int) time.Duration {
	window := retryBaseDelay << (attempt - 1)
	if window > retryMaxDelay || window <= 0 {
		window = retryMaxDelay
	}
	return rand.N(window + 1) //nolint:gosec // jitter, not security sensitive
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

func runTx(ctx context.Context, b Beginner, actor string, fn TxFunc) (err error) {
	tx, err := b.Begin(ctx)
	if err != nil {
		return fmt.Errorf("db: begin: %w", sanitize(err))
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	q := store.New(tx)
	if err = q.SetActor(ctx, actor); err != nil {
		return fmt.Errorf("db: set actor: %w", err)
	}
	if err = fn(ctx, q, tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit: %w", sanitize(err))
	}
	return nil
}

// SQLSTATE codes classified by this package.
const (
	codeSerializationFailure = "40001"
	codeDeadlockDetected     = "40P01"
	codeLockNotAvailable     = "55P03"
	codeQueryCanceled        = "57014"
)

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// IsRetryable reports whether err is a transient concurrency failure
// (serialization_failure or deadlock_detected) after which the whole
// transaction may be safely re-run. WithTx already retries these; seeing
// one from WithTx means the retries were exhausted.
func IsRetryable(err error) bool {
	c := pgCode(err)
	return c == codeSerializationFailure || c == codeDeadlockDetected
}

// IsLockTimeout reports lock_not_available (55P03): lock_timeout expired or
// NOWAIT found the row locked. Another request holds the lock; the client
// should retry later (503 busy + Retry-After), not immediately.
func IsLockTimeout(err error) bool { return pgCode(err) == codeLockNotAvailable }

// IsStatementTimeout reports query_canceled (57014): statement_timeout
// expired or an operator cancelled the query.
func IsStatementTimeout(err error) bool { return pgCode(err) == codeQueryCanceled }

// IsBusy reports any transient contention/overload failure that handlers
// should answer with 503 "busy" and Retry-After rather than 500: lock
// timeouts, statement timeouts and exhausted serialization/deadlock retries.
func IsBusy(err error) bool {
	switch pgCode(err) {
	case codeLockNotAvailable, codeQueryCanceled, codeSerializationFailure, codeDeadlockDetected:
		return true
	}
	return false
}

// IsNotFound reports whether err means no row matched.
func IsNotFound(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// ConstraintViolation returns the violated constraint name and SQLSTATE if
// err is an integrity error.
func ConstraintViolation(err error) (constraint, code string, ok bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) == 5 && pgErr.Code[:2] == "23" {
		return pgErr.ConstraintName, pgErr.Code, true
	}
	return "", "", false
}

// IsUniqueViolation reports a unique violation, optionally on a named constraint/index.
func IsUniqueViolation(err error, constraint string) bool {
	c, code, ok := ConstraintViolation(err)
	return ok && code == "23505" && (constraint == "" || c == constraint)
}

// IsCheckViolation reports a CHECK violation (or a trigger raising check_violation).
func IsCheckViolation(err error, constraint string) bool {
	c, code, ok := ConstraintViolation(err)
	return ok && code == "23514" && (constraint == "" || c == constraint)
}

// sanitize strips connection details that pgconn may embed in errors.
func sanitize(err error) error {
	var ce *pgconn.ConnectError
	if errors.As(err, &ce) {
		return errors.New("cannot connect to database" + ConnectHint(err))
	}
	return err
}

// ConnectHint names a likely cause of a connection failure without echoing
// anything from the URL (host, user and password stay out of logs).
func ConnectHint(err error) string {
	if err == nil {
		return ""
	}
	m := strings.ToLower(err.Error())
	switch {
	case strings.Contains(m, "server refused tls") || strings.Contains(m, "ssl is not enabled") ||
		strings.Contains(m, "does not support ssl"):
		return " (the server does not offer TLS: on the same private network set TSP_DB_SSLMODE=disable)"
	case strings.Contains(m, "password authentication failed"):
		return " (password authentication failed)"
	case strings.Contains(m, "no such host") || strings.Contains(m, "connection refused"):
		return " (host unreachable or not accepting connections)"
	}
	return ""
}

// slowQueryTracer logs slow statements. It logs the SQL text only (never the
// arguments, which may hold personal data).
type slowQueryTracer struct {
	logger    *slog.Logger
	threshold time.Duration
}

type traceKey struct{}

type traceData struct {
	start time.Time
	sql   string
}

func (t *slowQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceKey{}, traceData{start: time.Now(), sql: d.SQL})
}

func (t *slowQueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryEndData) {
	td, ok := ctx.Value(traceKey{}).(traceData)
	if !ok {
		return
	}
	if el := time.Since(td.start); el >= t.threshold {
		sql := td.sql
		if len(sql) > 300 {
			sql = sql[:300] + "..."
		}
		t.logger.WarnContext(ctx, "slow query", slog.Duration("duration", el), slog.String("sql", sql),
			slog.Bool("failed", d.Err != nil))
	}
}
