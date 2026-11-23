// Package db owns the PostgreSQL connection pool, the transaction helper and
// error classification. All writes that touch money go through WithTx, which
// runs at READ COMMITTED with explicit row locks (SELECT ... FOR UPDATE) taken
// in a fixed order: transaction -> payer -> payment requests.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// Pool is the shared connection pool.
type Pool = pgxpool.Pool

// Open creates and verifies a connection pool.
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
	rp := pc.ConnConfig.RuntimeParams
	rp["application_name"] = appName
	rp["statement_timeout"] = strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
	rp["idle_in_transaction_session_timeout"] = "60000"
	rp["lock_timeout"] = "10000"
	rp["timezone"] = "UTC"
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

// TxFunc runs inside a database transaction.
type TxFunc func(ctx context.Context, q *store.Queries, tx pgx.Tx) error

// Beginner is satisfied by *pgxpool.Pool and pgx.Tx (nested = savepoint).
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// WithTx runs fn in a READ COMMITTED transaction and commits if fn returns
// nil. actor ("cli:alice", "system:c2b-worker", "product:<id>") is recorded
// by triggers on every ledger status change in this transaction.
func WithTx(ctx context.Context, b Beginner, actor string, fn TxFunc) (err error) {
	if actor == "" {
		return errors.New("db: actor is required for every write transaction")
	}
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
		return errors.New("cannot connect to database")
	}
	return err
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
