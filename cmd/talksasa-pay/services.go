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
	"github.com/talksasa/talksasa-pay/internal/mpesa"
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
	// stk is always set; with M-Pesa off it has no Daraja client
	// (Available() is false) and mpesa is nil.
	stk   *stk.Service
	mpesa *mpesa.HTTPClient
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
	emitter := events.NewEmitter(apiview.View{PayBaseURL: cfg.Public.PayBaseURL, Paybill: cfg.Public.Paybill}, queue.NewEnqueuer(riverClient))
	adminCfg := admin.DefaultConfig()
	adminCfg.IdleTimeout = cfg.Admin.IdleTimeout
	adminCfg.AbsoluteTimeout = cfg.Admin.AbsoluteTimeout
	adminCfg.SetupBaseURL = cfg.Admin.SetupBaseURL
	pepper := []byte(cfg.Crypto.APIKeyPepper.Reveal())
	ledgerSvc := ledger.NewService(pool, logger)
	mc, err := newMpesaClient(cfg, logger, component)
	if err != nil {
		pool.Close()
		return nil, err
	}
	stkOpts := stk.Options{
		Pool: pool, Ledger: ledgerSvc, Events: emitter,
		Callbacks: queue.NewEnqueuer(riverClient), Logger: logger,
		QueryAfter: cfg.Mpesa.QueryAfter, GiveUpAfter: cfg.Mpesa.GiveUpAfter,
	}
	if mc != nil {
		// Only a non-nil client: a typed nil would make Available() true.
		stkOpts.Client = mc
	}
	stkSvc := stk.NewService(stkOpts)
	var prompter charge.Prompter
	if stkSvc.Available() {
		prompter = stkSvc
	}
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
		mpesa:     mc,
	}, nil
}

// newMpesaClient builds the Daraja client when M-Pesa is configured (nil,
// nil when it is off). Long-running components log which mode they run in;
// secrets are never logged.
func newMpesaClient(cfg *config.Config, logger *slog.Logger, component string) (*mpesa.HTTPClient, error) {
	m := cfg.Mpesa
	announce := component != "admin" // CLI commands stay quiet
	if !m.Enabled() {
		if announce {
			logger.Info("mpesa: off")
		}
		return nil, nil
	}
	c, err := mpesa.NewClient(mpesa.ClientOptions{
		Env: m.Env, BaseURL: m.BaseURL, ShortCode: m.ShortCode,
		ConsumerKey: m.ConsumerKey, ConsumerSecret: m.ConsumerSecret, Passkey: m.Passkey, CallbackToken: m.CallbackToken,
		CallbackBaseURL: m.CallbackBaseURL, Timeout: m.RequestTimeout, Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("mpesa client: %w", err)
	}
	if announce {
		logger.Info("mpesa: enabled", slog.String("env", m.Env), slog.String("shortcode", m.ShortCode),
			slog.String("callback_base", m.CallbackBaseURL), slog.Int("callback_cidrs", len(m.CallbackCIDRs)))
	}
	return c, nil
}

func (a *services) close() { a.pool.Close() }
