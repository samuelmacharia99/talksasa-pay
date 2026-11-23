package main

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
)

func runMigrate(args []string, logw io.Writer) error {
	if len(args) != 1 {
		return usageError{"usage: talksasa-pay migrate up|down|version"}
	}
	env, dsn, err := config.LoadMigrate(config.OSSource())
	if err != nil {
		return fmt.Errorf("configuration:\n%w", err)
	}
	logger := newLogger(logw, env, slog.LevelInfo).With(slog.String("component", "migrate"))
	m, err := db.NewMigrator(dsn, env, logger)
	if err != nil {
		return err
	}
	defer func() { _ = m.Close() }()

	switch args[0] {
	case "up":
		err = m.Up()
	case "down":
		err = m.Down()
	case "version":
	default:
		return usageError{"usage: talksasa-pay migrate up|down|version"}
	}
	if err != nil {
		return err
	}
	v, dirty, err := m.Version()
	if err != nil {
		return err
	}
	logger.Info("schema version", slog.Uint64("schema_version", uint64(v)), slog.Bool("dirty", dirty))
	if dirty {
		return fmt.Errorf("schema version %d is dirty: a migration failed part-way; fix manually before continuing", v)
	}
	return nil
}
