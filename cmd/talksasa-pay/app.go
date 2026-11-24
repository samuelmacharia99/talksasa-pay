package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/talksasa/talksasa-pay/internal/admin"
	"github.com/talksasa/talksasa-pay/internal/allocation"
	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/reporting"
)

// app holds the wired services shared by the api and admin commands.
type app struct {
	cfg             *config.Config
	logger          *slog.Logger
	pool            *db.Pool
	river           *queue.Client
	events          *events.Emitter
	products        *product.Service
	payers          *payer.Service
	paymentRequests *paymentrequest.Service
	ledger          *ledger.Service
	admin           *admin.Service
	reporting       *reporting.Service
}

func newApp(ctx context.Context, logw io.Writer, component string) (*app, error) {
	cfg, err := config.Load(config.OSSource())
	if err != nil {
		return nil, fmt.Errorf("configuration:\n%w", err)
	}
	logger := newLogger(logw, cfg.Env, cfg.LogLevel).With(slog.String("component", component))

	keys, err := crypto.ParseKeyring(cfg.Crypto.MasterKeys.Reveal(), cfg.Crypto.ActiveKeyVersion)
	if err != nil {
		return nil, err
	}
	pool, err := db.Open(ctx, cfg.Database, "talksasa-pay-"+component, logger)
	if err != nil {
		return nil, err
	}
	// Every money change emits its events (and webhook jobs) in the same
	// transaction. The River client here only inserts; the worker runs jobs.
	riverClient, err := queue.NewInsertOnly(pool, logger)
	if err != nil {
		pool.Close()
		return nil, err
	}
	emitter := events.NewEmitter(apiview.View{PayBaseURL: cfg.Public.PayBaseURL, Paybill: cfg.Public.Paybill}, queue.NewEnqueuer(riverClient))
	alloc := allocation.NewService(logger, emitter)
	adminCfg := admin.DefaultConfig()
	adminCfg.IdleTimeout = cfg.Admin.IdleTimeout
	adminCfg.AbsoluteTimeout = cfg.Admin.AbsoluteTimeout
	adminCfg.SetupBaseURL = cfg.Admin.SetupBaseURL
	pepper := []byte(cfg.Crypto.APIKeyPepper.Reveal())
	return &app{
		cfg:    cfg,
		logger: logger,
		pool:   pool,
		river:  riverClient,
		events: emitter,
		products: product.NewService(product.Options{
			Pool: pool, Keyring: keys, APIKeyPepper: pepper,
			AllowHTTPHooks: cfg.Env == config.EnvDevelopment, LiveKeys: cfg.Env == config.EnvProduction, Logger: logger,
		}),
		payers:          payer.NewService(pool, logger),
		paymentRequests: paymentrequest.NewService(pool, alloc, emitter, logger),
		ledger:          ledger.NewService(pool, alloc, logger),
		admin:           admin.NewService(admin.Options{Pool: pool, Keyring: keys, Pepper: pepper, Config: adminCfg, Logger: logger}),
		reporting:       reporting.NewService(pool),
	}, nil
}

func (a *app) close() { a.pool.Close() }
