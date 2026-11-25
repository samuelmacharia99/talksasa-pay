package db

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	// Registers the pgx/v5 database driver for golang-migrate.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	dbmigrations "github.com/talksasa/talksasa-pay/db"
	"github.com/talksasa/talksasa-pay/internal/config"
)

// Migrator applies the embedded schema migrations using the owner role.
type Migrator struct {
	m      *migrate.Migrate
	env    config.Environment
	logger *slog.Logger
}

// NewMigrator connects with the owner-role DSN.
func NewMigrator(dsn config.Secret, env config.Environment, logger *slog.Logger) (*Migrator, error) {
	src, err := iofs.New(dbmigrations.FS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("migrate: load embedded migrations: %w", err)
	}
	u, err := url.Parse(dsn.Reveal())
	if err != nil {
		return nil, errors.New("migrate: invalid database URL")
	}
	u.Scheme = "pgx5"
	m, err := migrate.NewWithSourceInstance("iofs", src, u.String())
	if err != nil {
		return nil, errors.New("migrate: cannot connect to database" + ConnectHint(err))
	}
	m.Log = migrateLogger{logger}
	return &Migrator{m: m, env: env, logger: logger}, nil
}

// Up applies all pending migrations.
func (mg *Migrator) Up() error {
	err := mg.m.Up()
	if errors.Is(err, migrate.ErrNoChange) {
		mg.logger.Info("schema already up to date")
		return nil
	}
	return err
}

// Down rolls back one migration. Refused outside development: down
// migrations drop ledger tables.
func (mg *Migrator) Down() error {
	if mg.env != config.EnvDevelopment {
		return fmt.Errorf("migrate: down migrations are disabled in %s", mg.env)
	}
	return mg.m.Steps(-1)
}

// Version reports the current schema version.
func (mg *Migrator) Version() (version uint, dirty bool, err error) {
	v, d, err := mg.m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return v, d, err
}

// Close releases the connection.
func (mg *Migrator) Close() error {
	srcErr, dbErr := mg.m.Close()
	return errors.Join(srcErr, dbErr)
}

type migrateLogger struct{ l *slog.Logger }

func (m migrateLogger) Printf(format string, v ...any) {
	m.l.Info("migrate", slog.String("detail", strings.TrimSpace(fmt.Sprintf(format, v...))))
}
func (m migrateLogger) Verbose() bool { return false }
