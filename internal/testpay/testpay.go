// Package testpay sends real test payments from the Settings page.
//
// A test is a real charge on the reserved internal "Gateway test" app
// (app.EnsureGatewayTestApp) with an M-Pesa prompt to the phone the admin
// typed. It goes through exactly the code an app's charge does (prompt,
// callback, receipt, ledger), so a passing test proves the paybill works
// end to end. The money is real and lands in the paybill; it is recorded and
// reported under the Gateway test app, never under a real app. Refunds, if
// wanted, are made by hand in the M-Pesa portal.
//
// Rules (approved 2026-11-26): admins only (settings.manage, enforced by the
// admin API), KES 1 to 1,000, and in production the admin types
// "production" to confirm, as when saving production settings.
package testpay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/charge"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/mpesasettings"
	"github.com/talksasa/talksasa-pay/internal/stk"
)

// Limits.
const (
	MinAmount   = 1
	MaxAmount   = 1_000
	RecentLimit = 10
	// The test charge is left open this long, so a customer who pays by
	// paybill after a failed prompt is still matched to it.
	expiresIn = time.Hour
)

var (
	ErrInvalidAmount = fmt.Errorf("testpay: amount must be a whole number of KES from %d to %d", MinAmount, MaxAmount)
	ErrInvalidPhone  = errors.New("testpay: phone must be a Kenyan Safaricom number")
	ErrInvalidKey    = errors.New("testpay: the form key is missing or invalid; reload the page")
	// ErrConfirm: production settings, and "production" was not typed.
	ErrConfirm = mpesasettings.ErrConfirm
	// ErrUnavailable: M-Pesa prompts cannot be sent with the settings in
	// effect (none saved, or incomplete).
	ErrUnavailable = stk.ErrUnavailable
	// ErrAppNotActive: the Gateway test app has been suspended.
	ErrAppNotActive = charge.ErrAppNotActive
)

// Apps finds or creates the reserved app (*app.Service).
type Apps interface {
	EnsureGatewayTestApp(ctx context.Context, actor audit.Actor) (app.App, error)
	GatewayTestApp(ctx context.Context) (app.App, bool, error)
}

// Charges creates and lists charges (*charge.Service).
type Charges interface {
	Create(ctx context.Context, actor audit.Actor, appID uuid.UUID, p charge.CreateParams) (charge.Created, error)
	List(ctx context.Context, appID uuid.UUID, p charge.ListParams) ([]charge.Charge, error)
}

// Pushes reports whether prompts can be sent and lists a charge's prompts
// (*stk.Service).
type Pushes interface {
	Available() bool
	ListForCharge(ctx context.Context, chargeID uuid.UUID, limit int) ([]stk.Push, error)
}

// Settings says which paybill environment is in effect (*mpesasettings.Service).
type Settings interface {
	Status(ctx context.Context) (mpesasettings.Status, error)
}

// Options configure the service.
type Options struct {
	Apps     Apps
	Charges  Charges
	Pushes   Pushes
	Settings Settings
	Logger   *slog.Logger
	Now      func() time.Time // nil = time.Now
}

// Service runs test payments. Safe for concurrent use.
type Service struct{ o Options }

// NewService builds the service.
func NewService(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o: o}
}

// StartParams is one test payment.
type StartParams struct {
	Amount int64
	Phone  string
	// Confirm must be "production" when production settings are in effect.
	Confirm string
	// Key makes a double-submitted form send one prompt: the same key
	// returns the same test. The admin panel puts a fresh one in each form.
	Key string
}

// Test is a test payment: its charge and the latest prompt for it.
type Test struct {
	Charge      charge.Charge
	Push        *stk.Push
	Environment string
	// PromptError is why the prompt could not be sent (Start only). The
	// charge still exists and expires unpaid.
	PromptError error
}

// Start creates the test charge and sends the prompt.
func (s *Service) Start(ctx context.Context, actor audit.Actor, p StartParams) (Test, error) {
	if p.Amount < MinAmount || p.Amount > MaxAmount {
		return Test{}, ErrInvalidAmount
	}
	phone := strings.TrimSpace(p.Phone)
	if phone == "" {
		return Test{}, ErrInvalidPhone
	}
	key := strings.TrimSpace(p.Key)
	if len(key) < 8 || len(key) > 64 {
		return Test{}, ErrInvalidKey
	}
	st, err := s.o.Settings.Status(ctx)
	if err != nil {
		return Test{}, err
	}
	if !st.Ready || !s.o.Pushes.Available() {
		return Test{}, ErrUnavailable
	}
	if st.Environment == config.MpesaProduction && strings.TrimSpace(p.Confirm) != mpesasettings.ConfirmProduction {
		return Test{}, ErrConfirm
	}
	a, err := s.o.Apps.EnsureGatewayTestApp(ctx, actor)
	if err != nil {
		return Test{}, err
	}
	now := s.o.Now().UTC()
	created, err := s.o.Charges.Create(ctx, actor, a.ID, charge.CreateParams{
		// The key makes the reference stable for a resubmitted form.
		Reference:      "settings-test-" + now.Format("20060102") + "-" + key,
		Amount:         p.Amount,
		Description:    "Settings test payment (not a customer payment)",
		Phone:          phone,
		ExpiresIn:      expiresIn,
		IdempotencyKey: "settings-test:" + key,
	})
	switch {
	case errors.Is(err, charge.ErrInvalidPhone):
		return Test{}, ErrInvalidPhone
	case errors.Is(err, charge.ErrDuplicateReference), errors.Is(err, charge.ErrIdempotencyConflict):
		// The same form key with a different amount or phone.
		return Test{}, ErrInvalidKey
	case err != nil:
		return Test{}, err
	}
	out := Test{Charge: created.Charge, Push: created.Push, Environment: st.Environment, PromptError: created.PushErr}
	s.o.Logger.InfoContext(ctx, "settings test payment started",
		slog.String("charge_id", created.Charge.ID.String()), slog.Int64("amount", p.Amount),
		slog.String("environment", st.Environment), slog.String("actor", actor.String()),
		slog.Bool("replayed", created.Replayed))
	return out, nil
}

// Recent lists the latest test payments, newest first, each with its latest
// prompt. Empty until the first test creates the Gateway test app.
func (s *Service) Recent(ctx context.Context, limit int) ([]Test, error) {
	if limit <= 0 || limit > RecentLimit {
		limit = RecentLimit
	}
	a, ok, err := s.o.Apps.GatewayTestApp(ctx)
	if err != nil || !ok {
		return nil, err
	}
	charges, err := s.o.Charges.List(ctx, a.ID, charge.ListParams{Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]Test, 0, len(charges))
	for _, c := range charges {
		t := Test{Charge: c}
		pushes, err := s.o.Pushes.ListForCharge(ctx, c.ID, 1)
		if err != nil {
			return nil, err
		}
		if len(pushes) > 0 {
			t.Push = &pushes[0]
		}
		out = append(out, t)
	}
	return out, nil
}
