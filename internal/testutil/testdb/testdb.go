// Package testdb gives integration tests a fresh, fully migrated PostgreSQL
// database each. Tests run as the restricted application role, exactly like
// production, with an owner-role pool available for tests that must prove a
// rule holds even for the schema owner.
//
// Set TSP_TEST_DATABASE_URL to an owner-role URL for the maintenance database
// (make test does this). Without it the tests skip, unless
// TSP_REQUIRE_DB_TESTS=1 (CI), in which case they fail.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // migrate driver
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	dbmigrations "github.com/talksasa/talksasa-pay/db"
)

// DB is one throwaway database.
type DB struct {
	App   *pgxpool.Pool // connects as talksasa_app (no DELETE/TRUNCATE)
	Owner *pgxpool.Pool // schema owner
	Name  string
}

// New creates, migrates and registers cleanup for a fresh database.
func New(t testing.TB) *DB {
	t.Helper()
	adminURL := os.Getenv("TSP_TEST_DATABASE_URL")
	if adminURL == "" {
		if os.Getenv("TSP_REQUIRE_DB_TESTS") == "1" {
			t.Fatal("TSP_TEST_DATABASE_URL is required (TSP_REQUIRE_DB_TESTS=1)")
		}
		t.Skip("TSP_TEST_DATABASE_URL not set; skipping database test (run `make test`)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "tsp_test_" + hex.EncodeToString(b)

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("testdb: connect admin: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer ccancel()
		c, err := pgx.Connect(cctx, adminURL)
		if err != nil {
			t.Logf("testdb: cleanup connect: %v", err)
			return
		}
		defer func() { _ = c.Close(cctx) }()
		if _, err := c.Exec(cctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("testdb: drop %s: %v", name, err)
		}
	})

	dbURL, err := withDatabase(adminURL, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateUp(dbURL); err != nil {
		t.Fatalf("testdb: migrate: %v", err)
	}

	owner, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatalf("testdb: owner pool: %v", err)
	}
	appCfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	appCfg.MaxConns = 60
	appCfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	appCfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE talksasa_app")
		return err
	}
	app, err := pgxpool.NewWithConfig(ctx, appCfg)
	if err != nil {
		t.Fatalf("testdb: app pool: %v", err)
	}
	t.Cleanup(func() { app.Close(); owner.Close() })
	return &DB{App: app, Owner: owner, Name: name}
}

func withDatabase(raw, name string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("testdb: invalid TSP_TEST_DATABASE_URL")
	}
	u.Path = "/" + name
	return u.String(), nil
}

func migrateUp(dbURL string) error {
	src, err := iofs.New(dbmigrations.FS, "migrations")
	if err != nil {
		return err
	}
	u, _ := url.Parse(dbURL)
	u.Scheme = "pgx5"
	m, err := migrate.NewWithSourceInstance("iofs", src, u.String())
	if err != nil {
		return err
	}
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}
