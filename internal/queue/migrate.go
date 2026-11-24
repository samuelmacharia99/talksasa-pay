package queue

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"github.com/talksasa/talksasa-pay/internal/config"
)

// Migrate applies River's own schema migrations with the owner role, then
// grants the application role what River needs at runtime. Idempotent; run
// by `talksasa-pay migrate up` after our migrations.
func Migrate(ctx context.Context, dsn config.Secret, logger *slog.Logger) error {
	pool, err := pgxpool.New(ctx, dsn.Reveal())
	if err != nil {
		return fmt.Errorf("queue: connect for migration: invalid database URL")
	}
	defer pool.Close()
	m, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Logger: logger})
	if err != nil {
		return fmt.Errorf("queue: migrator: %w", err)
	}
	res, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return fmt.Errorf("queue: river migrations: %w", err)
	}
	logger.Info("river schema up to date", slog.Int("applied", len(res.Versions)))

	// River deletes finished jobs itself (job cleaner), so its tables need
	// DELETE. Our ledger tables never grant it.
	_, err = pool.Exec(ctx, `
DO $$
DECLARE t text;
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'talksasa_app') THEN
    RAISE WARNING 'role talksasa_app does not exist; skipping River grants';
    RETURN;
  END IF;
  FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename LIKE 'river\_%' AND tablename <> 'river_migration' LOOP
    EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON public.%I TO talksasa_app', t);
  END LOOP;
  GRANT SELECT ON public.river_migration TO talksasa_app;
  FOR t IN SELECT sequencename FROM pg_sequences WHERE schemaname = 'public' AND sequencename LIKE 'river\_%' LOOP
    EXECUTE format('GRANT USAGE, SELECT ON SEQUENCE public.%I TO talksasa_app', t);
  END LOOP;
END $$;`)
	if err != nil {
		return fmt.Errorf("queue: grant river tables: %w", err)
	}
	return nil
}
