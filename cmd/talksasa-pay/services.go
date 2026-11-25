package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/talksasa/talksasa-pay/internal/admin"
	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/charge"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/ledger"
	"github.com/talksasa/talksasa-pay/internal/mpesasettings"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/stk"
)

// services holds the wired services shared by the api and admin commands.
type services struct {
	cfg       *config.Config
	logger    *slog.Logger
	pool      *db.Pool
	river     *queue.Client
	events    *events.Emitter
	apps      *app.Service
	charges   *charge.Service
	ledger    *ledger.Service
	admin     *admin.Service
	reporting *reporting.Service
	// stk is always set. Its Daraja client is the settings provider:
	// Available() follows the paybill saved in the admin panel (or the
	// TSP_MPESA_* fallback) and is false while M-Pesa is off.
	stk *stk.Service
	// mpesaSettings edits the paybill settings; mpesa serves the current
	// Daraja client and paybill to the api and worker.
	mpesaSettings *mpesasettings.Service
	mpesa         *mpesasettings.Provider
}

func newApp(ctx context.Context, logw io.Writer, component string) (*services, error) {
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
	// M-Pesa settings: saved in the admin panel (database) or, failing
	// that, TSP_MPESA_*. The provider rebuilds the Daraja client on change.
	settingsLog := logger
	if component == "admin" {
		settingsLog = slog.New(slog.DiscardHandler) // CLI commands stay quiet
	}
	mpesaSettings := mpesasettings.NewService(mpesasettings.Options{
		Pool: pool, Keyring: keys, Env: cfg.Env, Mpesa: cfg.Mpesa, Paybill: cfg.Public.Paybill, Logger: settingsLog,
	})
	mpesaProvider := mpesasettings.NewProvider(ctx, mpesaSettings, 0)
	emitter := events.NewEmitter(apiview.View{PayBaseURL: cfg.Public.PayBaseURL, PaybillFrom: mpesaProvider.PaybillShortCode}, queue.NewEnqueuer(riverClient))
	adminCfg := admin.DefaultConfig()
	adminCfg.IdleTimeout = cfg.Admin.IdleTimeout
	adminCfg.AbsoluteTimeout = cfg.Admin.AbsoluteTimeout
	adminCfg.SetupBaseURL = cfg.Admin.SetupBaseURL
	pepper := []byte(cfg.Crypto.APIKeyPepper.Reveal())
	ledgerSvc := ledger.NewService(pool, logger)
	stkSvc := stk.NewService(stk.Options{
		Pool: pool, Ledger: ledgerSvc, Events: emitter, Client: mpesaProvider,
		Callbacks: queue.NewEnqueuer(riverClient), Logger: logger,
		QueryAfter: cfg.Mpesa.QueryAfter, GiveUpAfter: cfg.Mpesa.GiveUpAfter,
	})
	// Always the STK service: while M-Pesa is off, Send answers
	// ErrUnavailable and the charge is created without a prompt.
	var prompter charge.Prompter = stkSvc
	return &services{
		cfg:    cfg,
		logger: logger,
		pool:   pool,
		river:  riverClient,
		events: emitter,
		apps: app.NewService(app.Options{
			Pool: pool, Keyring: keys, APIKeyPepper: pepper,
			AllowHTTPHooks: cfg.Env == config.EnvDevelopment, LiveKeys: cfg.Env == config.EnvProduction, Logger: logger,
		}),
		charges:   charge.NewService(charge.Options{Pool: pool, Events: emitter, STK: prompter, Logger: logger}),
		ledger:    ledgerSvc,
		admin:     admin.NewService(admin.Options{Pool: pool, Keyring: keys, Pepper: pepper, Config: adminCfg, Logger: logger}),
		reporting: reporting.NewService(pool),
		stk:       stkSvc,

		mpesaSettings: mpesaSettings,
		mpesa:         mpesaProvider,
	}, nil
}

func (a *services) close() { a.pool.Close() }
