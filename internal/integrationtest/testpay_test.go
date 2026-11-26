package integrationtest

// Settings test payments: a real charge on the reserved Gateway test app,
// paid through the ordinary STK callback path.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/mpesasettings"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testpay"
)

type fixedSettings struct{ st mpesasettings.Status }

func (f fixedSettings) Status(context.Context) (mpesasettings.Status, error) { return f.st, nil }

func newTestPay(m *mpesaEnv, env string, ready bool) *testpay.Service {
	return testpay.NewService(testpay.Options{
		Apps: m.apps, Charges: m.charges, Pushes: m.svc, Logger: m.logger, Now: m.now,
		Settings: fixedSettings{mpesasettings.Status{Environment: env, Ready: ready}},
	})
}

func TestSettingsTestPayment(t *testing.T) {
	m := newMpesaEnv(t, nil, nil)
	tp := newTestPay(m, config.MpesaSandbox, true)
	staff := mpesaStaff()

	t.Run("validates before creating anything", func(t *testing.T) {
		cases := []struct {
			p    testpay.StartParams
			want error
		}{
			{testpay.StartParams{Amount: 0, Phone: newPhone(0), Key: uuid.NewString()}, testpay.ErrInvalidAmount},
			{testpay.StartParams{Amount: 1001, Phone: newPhone(0), Key: uuid.NewString()}, testpay.ErrInvalidAmount},
			{testpay.StartParams{Amount: 10, Phone: "", Key: uuid.NewString()}, testpay.ErrInvalidPhone},
			{testpay.StartParams{Amount: 10, Phone: "0712", Key: uuid.NewString()}, testpay.ErrInvalidPhone},
			{testpay.StartParams{Amount: 10, Phone: newPhone(0), Key: "short"}, testpay.ErrInvalidKey},
		}
		before := m.client.pushCount()
		for _, c := range cases {
			if _, err := tp.Start(m.ctx, staff, c.p); !errors.Is(err, c.want) {
				t.Errorf("%+v: got %v, want %v", c.p, err, c.want)
			}
		}
		if m.client.pushCount() != before {
			t.Fatal("an invalid test sent a prompt")
		}
	})

	t.Run("production needs the typed confirmation", func(t *testing.T) {
		prod := newTestPay(m, config.MpesaProduction, true)
		if _, err := prod.Start(m.ctx, staff, testpay.StartParams{Amount: 5, Phone: newPhone(0), Key: uuid.NewString()}); !errors.Is(err, testpay.ErrConfirm) {
			t.Fatalf("got %v, want ErrConfirm", err)
		}
		tt, err := prod.Start(m.ctx, staff, testpay.StartParams{Amount: 5, Phone: newPhone(0), Key: uuid.NewString(), Confirm: "production"})
		if err != nil || tt.Push == nil {
			t.Fatalf("confirmed production test: %v", err)
		}
	})

	t.Run("not ready sends nothing", func(t *testing.T) {
		off := newTestPay(m, config.MpesaSandbox, false)
		if _, err := off.Start(m.ctx, staff, testpay.StartParams{Amount: 5, Phone: newPhone(0), Key: uuid.NewString()}); !errors.Is(err, testpay.ErrUnavailable) {
			t.Fatalf("got %v, want ErrUnavailable", err)
		}
	})

	t.Run("pays through the real callback path on the Gateway test app", func(t *testing.T) {
		ph := newPhone(0)
		key := uuid.NewString()
		tt, err := tp.Start(m.ctx, staff, testpay.StartParams{Amount: 7, Phone: ph, Key: key})
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		if tt.PromptError != nil || tt.Push == nil || tt.Push.Status != store.StkPushStatusSent {
			t.Fatalf("prompt: err=%v push=%+v", tt.PromptError, tt.Push)
		}
		a, ok, err := m.apps.GatewayTestApp(m.ctx)
		if err != nil || !ok {
			t.Fatalf("gateway test app: ok=%v err=%v", ok, err)
		}
		if !a.GatewayTest || a.AccountPrefix != app.GatewayTestPrefix || tt.Charge.AppID != a.ID {
			t.Fatalf("charge on app %s, gateway test app %+v", tt.Charge.AppID, a)
		}

		// A double-submitted form returns the same test and sends no new prompt.
		n := m.client.pushCount()
		again, err := tp.Start(m.ctx, staff, testpay.StartParams{Amount: 7, Phone: ph, Key: key})
		if err != nil || again.Charge.ID != tt.Charge.ID || m.client.pushCount() != n {
			t.Fatalf("replay: err=%v same=%v prompts %d -> %d", err, again.Charge.ID == tt.Charge.ID, n, m.client.pushCount())
		}
		// The same key with another amount is refused.
		if _, err := tp.Start(m.ctx, staff, testpay.StartParams{Amount: 8, Phone: ph, Key: key}); !errors.Is(err, testpay.ErrInvalidKey) {
			t.Fatalf("reused key: %v", err)
		}

		co, _ := m.client.last()
		m.mustDeliver(store.MpesaCallbackKindStk, stkSuccessBody(co, newMpesaReceipt(), 7, ph))
		m.assertCharge(tt.Charge.ID, store.ChargeStatusSucceeded)
		if n, sum := m.succeededFor(tt.Charge.ID); n != 1 || sum != 7 {
			t.Fatalf("%d payments totalling %d", n, sum)
		}

		recent, err := tp.Recent(m.ctx, 10)
		if err != nil || len(recent) == 0 {
			t.Fatalf("recent: %v (%d)", err, len(recent))
		}
		var found bool
		for _, r := range recent {
			if r.Charge.ID == tt.Charge.ID {
				found = true
				if r.Charge.Status != store.ChargeStatusSucceeded || r.Push == nil || r.Push.Status != store.StkPushStatusSucceeded {
					t.Fatalf("recent shows %s / %+v", r.Charge.Status, r.Push)
				}
			}
		}
		if !found {
			t.Fatal("the test is not in Recent")
		}
	})

	t.Run("concurrent first tests share one Gateway test app", func(t *testing.T) {
		var wg sync.WaitGroup
		ids := make([]uuid.UUID, 8)
		errs := make([]error, 8)
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				a, err := m.apps.EnsureGatewayTestApp(m.ctx, staff)
				ids[i], errs[i] = a.ID, err
			}()
		}
		wg.Wait()
		for i := range ids {
			if errs[i] != nil || ids[i] != ids[0] {
				t.Fatalf("ensure %d: %v (%s vs %s)", i, errs[i], ids[i], ids[0])
			}
		}
		var n int
		if err := m.db.Owner.QueryRow(m.ctx, "SELECT count(*) FROM apps WHERE gateway_test").Scan(&n); err != nil || n != 1 {
			t.Fatalf("%d gateway test apps (%v)", n, err)
		}
		if _, err := m.db.Owner.Exec(m.ctx, "UPDATE apps SET gateway_test = false WHERE gateway_test"); err == nil {
			t.Fatal("gateway_test could be cleared")
		}
	})
}
