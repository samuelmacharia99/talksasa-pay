package mpesasettings

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/mpesa/mpesatest"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

const (
	cbToken   = "cbtok_0123456789abcdefghijklmnopqrstuvwxyz"
	conKey    = "ck_live_ABCDEFGHIJKLMNOP_key1"
	conSecret = "cs_live_QRSTUVWXYZ_secret9876"
	passkey   = "pk_bfb279f9aa9bdbcf158e97dd71a467cd2e0c893059b10f78e6b72ada1ed2c919"
)

var actor = audit.CLI("Jane Ops")

type env struct {
	d    *testdb.DB
	svc  *Service
	fake *mpesatest.Server
	logs *bytes.Buffer
}

func newEnv(t *testing.T, mutate ...func(*Options)) *env {
	t.Helper()
	d := testdb.New(t)
	line, err := crypto.GenerateKeyLine(1)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := crypto.ParseKeyring(line, 0)
	if err != nil {
		t.Fatal(err)
	}
	fake := mpesatest.New(t, mpesatest.Options{ConsumerKey: conKey, ConsumerSecret: conSecret})
	var logs bytes.Buffer
	o := Options{
		Pool: d.App, Keyring: keys, Env: config.EnvDevelopment,
		Mpesa:  config.MpesaConfig{BaseURL: fake.URL, CallbackToken: config.NewSecret(cbToken), RequestTimeout: 5 * time.Second},
		Logger: tlog.New(&logs, tlog.Options{Level: slog.LevelDebug}),
	}
	for _, m := range mutate {
		m(&o)
	}
	return &env{d: d, svc: NewService(o), fake: fake, logs: &logs}
}

func good() SaveParams {
	return SaveParams{Environment: "sandbox", Paybill: "174379", CallbackBaseURL: "http://api:8088/",
		ConsumerKey: config.NewSecret(conKey), ConsumerSecret: config.NewSecret(conSecret), Passkey: config.NewSecret(passkey)}
}

func (e *env) auditRows(t *testing.T, action string) []string {
	t.Helper()
	rows, err := e.d.Owner.Query(context.Background(), `SELECT details::text FROM audit_log WHERE action = $1 ORDER BY id`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// noSecrets fails if any secret value appears in s.
func noSecrets(t *testing.T, where, s string) {
	t.Helper()
	for _, v := range []string{conKey, conSecret, passkey, cbToken, "wrong-secret-value-123"} {
		if strings.Contains(s, v) {
			t.Fatalf("%s leaks a secret (%.6s…): %s", where, v, s)
		}
	}
}

func TestSaveAndStatus(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	st, err := e.svc.Status(ctx)
	if err != nil || st.Source != SourceNone || st.Ready || !st.CallbackTokenSet {
		t.Fatalf("initial %+v %v", st, err)
	}

	// The first save needs every credential.
	p := good()
	p.Passkey = config.Secret{}
	var ve *ValidationError
	if _, err := e.svc.Save(ctx, actor, p); !errors.As(err, &ve) || ve.Field != "passkey" {
		t.Fatalf("missing passkey: %v", err)
	}
	for name, mut := range map[string]func(*SaveParams){
		"environment":       func(p *SaveParams) { p.Environment = "production" }, // development deployment
		"shortcode":         func(p *SaveParams) { p.Paybill = "12ab" },
		"stk_shortcode":     func(p *SaveParams) { p.STKShortCode = "1" },
		"callback_base_url": func(p *SaveParams) { p.CallbackBaseURL = "https://api.example/cb?x=1" },
		"consumer_key":      func(p *SaveParams) { p.ConsumerKey = config.NewSecret("has space") },
	} {
		p := good()
		mut(&p)
		if _, err := e.svc.Save(ctx, actor, p); !errors.As(err, &ve) || ve.Field != name {
			t.Errorf("%s: %v", name, err)
		}
	}
	p = good()
	p.CallbackBaseURL = "https://api.example"
	if _, err := e.svc.Save(ctx, actor, p); !errors.As(err, &ve) || ve.Field != "callback_base_url" {
		t.Errorf("no trailing slash: %v", err)
	}

	st, err = e.svc.Save(ctx, actor, good())
	if err != nil {
		t.Fatal(err)
	}
	if st.Source != SourceDatabase || st.Version != 1 || !st.Ready || st.Paybill != "174379" || st.STKShortCode != "" ||
		st.ConsumerKey != (Credential{true, "key1"}) || st.ConsumerSecret != (Credential{true, "9876"}) || st.Passkey.Last4 != "c919" ||
		st.CallbackURLs.Confirmation != "http://api:8088/cb/hooks/[TOKEN]/c2b/confirmation" {
		t.Fatalf("saved status %+v", st)
	}

	// Encrypted at rest.
	var raw string
	if err := e.d.Owner.QueryRow(ctx, `SELECT row_to_json(m)::text FROM mpesa_settings m`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	noSecrets(t, "the database row", raw)

	// Empty credentials keep the stored ones; only the changed field is audited.
	p = SaveParams{Environment: "sandbox", Paybill: "600999", STKShortCode: "174379", CallbackBaseURL: "http://api:8088/"}
	st, err = e.svc.Save(ctx, actor, p)
	if err != nil || st.Version != 2 || st.Paybill != "600999" || st.STKShortCode != "174379" || !st.Passkey.Set {
		t.Fatalf("update %+v %v", st, err)
	}
	got, err := e.svc.Get(ctx)
	if err != nil || got.ConsumerKey.Reveal() != conKey || got.ConsumerSecret.Reveal() != conSecret || got.Passkey.Reveal() != passkey {
		t.Fatalf("kept credentials: %v", err)
	}
	// Identical: nothing written.
	if st, err = e.svc.Save(ctx, actor, p); err != nil || st.Version != 2 {
		t.Fatalf("no-op save %+v %v", st, err)
	}
	// A new secret is a change.
	p.ConsumerSecret = config.NewSecret("wrong-secret-value-123")
	if st, err = e.svc.Save(ctx, actor, p); err != nil || st.Version != 3 {
		t.Fatalf("secret change %+v %v", st, err)
	}

	rows := e.auditRows(t, "mpesa_settings.updated")
	if len(rows) != 3 || !strings.Contains(rows[0], `"first_save": true`) || !strings.Contains(rows[0], `"passkey"`) ||
		!strings.Contains(rows[1], `"changed": ["shortcode", "stk_shortcode"]`) || !strings.Contains(rows[2], `"changed": ["consumer_secret"]`) {
		t.Fatalf("audit rows: %q", rows)
	}
	for _, r := range rows {
		noSecrets(t, "audit details", r)
	}
	noSecrets(t, "logs", e.logs.String())

	// The row cannot be deleted by the application role.
	if _, err := e.d.App.Exec(ctx, `DELETE FROM mpesa_settings`); err == nil {
		t.Fatal("settings row deleted")
	}
}

func TestProductionRules(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Env = config.EnvStaging })
	ctx := context.Background()
	var ve *ValidationError

	p := good()
	if _, err := e.svc.Save(ctx, actor, p); !errors.As(err, &ve) || ve.Field != "callback_base_url" {
		t.Fatalf("http outside development: %v", err)
	}
	p.CallbackBaseURL = "https://api.pay.example/"
	p.Environment = "production"
	if _, err := e.svc.Save(ctx, actor, p); !errors.As(err, &ve) || ve.Field != "environment" {
		t.Fatalf("production without callback CIDRs: %v", err)
	}

	e = newEnv(t, func(o *Options) {
		o.Env = config.EnvProduction
		o.Mpesa.CallbackCIDRs = append(o.Mpesa.CallbackCIDRs, mustPrefix(t, "196.201.214.0/24"))
	})
	p = good()
	p.CallbackBaseURL = "https://api.pay.example/"
	if _, err := e.svc.Save(ctx, actor, p); !errors.As(err, &ve) || ve.Field != "environment" {
		t.Fatalf("sandbox in production: %v", err)
	}
	p.Environment = "production"
	if _, err := e.svc.Save(ctx, actor, p); !errors.Is(err, ErrConfirm) {
		t.Fatalf("unconfirmed production save: %v", err)
	}
	p.Confirm = ConfirmProduction
	if _, err := e.svc.Save(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RegisterURLs(ctx, actor, ""); !errors.Is(err, ErrConfirm) {
		t.Fatalf("unconfirmed production registration: %v", err)
	}
	if _, err := e.svc.RegisterURLs(ctx, actor, ConfirmProduction); err != nil {
		t.Fatal(err)
	}
	if reg, ok := e.fake.RegisteredURLs(); !ok || reg.Version != "v2" {
		t.Fatalf("production registration %+v", reg)
	}
}

func TestTestAndRegisterURLs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := e.svc.Test(ctx, actor); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("test with nothing saved: %v", err)
	}
	if _, err := e.svc.Save(ctx, actor, good()); err != nil {
		t.Fatal(err)
	}

	res, err := e.svc.Test(ctx, actor)
	if err != nil || !res.OK || res.Message != "" {
		t.Fatalf("test %+v %v", res, err)
	}
	st, _ := e.svc.Status(ctx)
	if st.LastTest == nil || !st.LastTest.OK || st.Version != 1 {
		t.Fatalf("last test %+v (version %d)", st.LastTest, st.Version)
	}

	reg, err := e.svc.RegisterURLs(ctx, actor, "")
	if err != nil {
		t.Fatal(err)
	}
	sent, ok := e.fake.RegisteredURLs()
	if !ok || sent.ShortCode != "174379" || sent.ResponseType != "Completed" ||
		sent.ConfirmationURL != "http://api:8088/cb/hooks/"+cbToken+"/c2b/confirmation" {
		t.Fatalf("Daraja got %+v", sent)
	}
	if reg.ConfirmationURL != "http://api:8088/cb/hooks/[TOKEN]/c2b/confirmation" {
		t.Fatalf("result %+v", reg)
	}
	st, _ = e.svc.Status(ctx)
	if st.Registration == nil || !st.Registration.Current || st.Registration.By != actor.String() || st.Version != 1 {
		t.Fatalf("registration %+v", st.Registration)
	}
	// Moving the callback base makes the registration stale.
	p := good()
	p.CallbackBaseURL = "http://api2:8088/"
	if st, err = e.svc.Save(ctx, actor, p); err != nil || st.Registration == nil || st.Registration.Current {
		t.Fatalf("stale registration %+v %v", st.Registration, err)
	}

	// Wrong credentials: a failed test, not an error.
	p.ConsumerSecret = config.NewSecret("wrong-secret-value-123")
	if _, err := e.svc.Save(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	res, err = e.svc.Test(ctx, actor)
	if err != nil || res.OK || res.Message == "" {
		t.Fatalf("bad credentials %+v %v", res, err)
	}
	noSecrets(t, "test message", res.Message)
	var de *DarajaError
	if _, err := e.svc.RegisterURLs(ctx, actor, ""); !errors.As(err, &de) {
		t.Fatalf("register with bad credentials: %v", err)
	}
	noSecrets(t, "register error", de.Error())

	for _, action := range []string{"mpesa_settings.tested", "mpesa.paybill_urls_registered"} {
		rows := e.auditRows(t, action)
		if len(rows) == 0 {
			t.Fatalf("no %s audit", action)
		}
		for _, r := range rows {
			noSecrets(t, action+" audit", r)
		}
	}
	var stored string
	if err := e.d.Owner.QueryRow(ctx, `SELECT urls_registered_url FROM mpesa_settings`).Scan(&stored); err != nil || strings.Contains(stored, cbToken) {
		t.Fatalf("stored registration URL %q %v", stored, err)
	}
	noSecrets(t, "logs", e.logs.String())
}

// Without TSP_MPESA_CALLBACK_TOKEN the first save generates a token and
// stores it encrypted; it never changes after that, is never shown, logged
// or audited, and the provider serves it to the callback intake.
func TestGeneratedCallbackToken(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Mpesa.CallbackToken = config.Secret{} })
	ctx := context.Background()

	st, err := e.svc.Status(ctx)
	if err != nil || st.CallbackTokenSet || st.CallbackTokenSource != "" {
		t.Fatalf("before save: %+v %v", st, err)
	}
	st, err = e.svc.Save(ctx, actor, good())
	if err != nil || !st.Ready || !st.CallbackTokenSet || st.CallbackTokenSource != "generated" {
		t.Fatalf("save: %+v %v", st, err)
	}
	got, err := e.svc.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tok := got.CallbackToken.Reveal()
	if len(tok) != 43 || strings.Trim(tok, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") != "" {
		t.Fatalf("token shape %d %q", len(tok), tok[:4])
	}
	var ct []byte
	if err := e.d.Owner.QueryRow(ctx, `SELECT callback_token_ciphertext FROM mpesa_settings`).Scan(&ct); err != nil || len(ct) == 0 ||
		bytes.Contains(ct, []byte(tok)) {
		t.Fatalf("stored token: %d bytes, %v", len(ct), err)
	}

	// A later save, including a credential change, keeps the token.
	s := good()
	s.Paybill = "600111"
	s.ConsumerKey = config.NewSecret("ck_live_ROTATED_0000_key2")
	if _, err := e.svc.Save(ctx, actor, s); err != nil {
		t.Fatal(err)
	}
	again, err := e.svc.Get(ctx)
	if err != nil || again.CallbackToken.Reveal() != tok {
		t.Fatalf("token changed on a later save: %v", err)
	}

	p := NewProvider(ctx, e.svc, time.Hour)
	if p.CallbackToken().Reveal() != tok {
		t.Fatal("provider does not serve the saved token")
	}
	if _, err := e.svc.Test(ctx, actor); err != nil {
		t.Fatalf("test: %v", err)
	}

	audits := strings.Join(e.auditRows(t, "mpesa_settings.updated"), "\n")
	if !strings.Contains(audits, `"callback_token"`) {
		t.Fatalf("first save does not record that a token was generated: %s", audits)
	}
	for where, v := range map[string]string{"logs": e.logs.String(), "audit": audits} {
		if strings.Contains(v, tok) {
			t.Fatalf("%s contain the generated callback token", where)
		}
	}
}

// TSP_MPESA_CALLBACK_TOKEN, when set, is the token in effect.
func TestDeploymentCallbackTokenWins(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	st, err := e.svc.Save(ctx, actor, good())
	if err != nil || st.CallbackTokenSource != "deployment" {
		t.Fatalf("%+v %v", st, err)
	}
	got, err := e.svc.Get(ctx)
	if err != nil || got.CallbackToken.Reveal() != cbToken {
		t.Fatalf("effective token is not the deployment's: %v", err)
	}
	if p := NewProvider(ctx, e.svc, time.Hour); p.CallbackToken().Reveal() != cbToken {
		t.Fatal("provider token")
	}
}

// TSP_MPESA_CALLBACK_BASE_URL, when set, fixes the callback base URL.
func TestFixedCallbackBaseURL(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Mpesa.CallbackBaseURL = "http://fixed.example:8088/" })
	ctx := context.Background()
	s := good()
	s.CallbackBaseURL = "http://derived.example/api/"
	st, err := e.svc.Save(ctx, actor, s)
	if err != nil || !st.CallbackBaseURLFixed || st.CallbackBaseURL != "http://fixed.example:8088/" {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestProviderFollowsSettings(t *testing.T) {
	// Environment fallback first.
	e := newEnv(t, func(o *Options) {
		o.Paybill = "174379"
		o.Mpesa.Env = config.MpesaSandbox
		o.Mpesa.ShortCode = "174379"
		o.Mpesa.CallbackBaseURL = "http://api:8088/"
		o.Mpesa.ConsumerKey, o.Mpesa.ConsumerSecret, o.Mpesa.Passkey = config.NewSecret(conKey), config.NewSecret(conSecret), config.NewSecret(passkey)
	})
	ctx := context.Background()
	p := NewProvider(ctx, e.svc, time.Hour)
	if !p.Configured() || p.PaybillShortCode() != "174379" || p.Source() != SourceEnvironment {
		t.Fatalf("env fallback: %v %q %s", p.Configured(), p.PaybillShortCode(), p.Source())
	}

	// A save in this process applies at once and the database wins.
	s := good()
	s.Paybill = "600111"
	if _, err := e.svc.Save(ctx, actor, s); err != nil {
		t.Fatal(err)
	}
	if p.PaybillShortCode() != "600111" || p.Source() != SourceDatabase {
		t.Fatalf("after save: %q %s", p.PaybillShortCode(), p.Source())
	}
	if _, err := p.STKPush(ctx, mpesa.STKPushRequest{Phone: "254708374149", Amount: 10, AccountReference: "HOST-1"}); err != nil {
		t.Fatalf("push through provider: %v", err)
	}

	// Another process (its own service, no notification) follows within
	// its recheck period.
	other := NewProvider(ctx, NewService(e.svc.o), 50*time.Millisecond)
	s.Paybill = "600222"
	if _, err := e.svc.Save(ctx, actor, s); err != nil {
		t.Fatal(err)
	}
	if other.PaybillShortCode() != "600111" {
		t.Fatal("re-read before the recheck period")
	}
	time.Sleep(60 * time.Millisecond)
	if other.PaybillShortCode() != "600222" {
		t.Fatalf("other process: %q", other.PaybillShortCode())
	}
	noSecrets(t, "logs", e.logs.String())
}

func TestProviderUnconfigured(t *testing.T) {
	e := newEnv(t)
	p := NewProvider(context.Background(), e.svc, time.Hour)
	if p.Configured() || p.PaybillShortCode() != "" {
		t.Fatal("configured with nothing saved")
	}
	_, err := p.STKPush(context.Background(), mpesa.STKPushRequest{Phone: "254708374149", Amount: 10})
	if !mpesa.IsRejected(err) || !errors.Is(err, mpesa.ErrUnavailable) {
		t.Fatalf("push: %v", err)
	}
	if _, err := p.STKQuery(context.Background(), "ws_CO_1"); !errors.Is(err, mpesa.ErrUnavailable) {
		t.Fatalf("query: %v", err)
	}
}

// Safaricom refuses C2B URLs containing words such as "mpesa", "cmd" or
// "sql" (400.003.02). The paths never contain them, a generated token
// never does, and a configured token that does is explained before Daraja
// is asked.
func TestCallbackURLsAvoidWordsSafaricomRefuses(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, func(o *Options) {
		o.Mpesa.CallbackToken = config.NewSecret("cbtok_xxCmDxx_0123456789abcdefghijklmnopq")
	})
	if _, err := e.svc.Save(ctx, actor, good()); err != nil {
		t.Fatal(err)
	}
	_, err := e.svc.RegisterURLs(ctx, actor, "")
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Field != "callback_token" || !strings.Contains(ve.Message, "TSP_MPESA_CALLBACK_TOKEN") {
		t.Fatalf("register with a refused token: %v", err)
	}
	if _, ok := e.fake.RegisteredURLs(); ok {
		t.Fatal("Daraja was asked anyway")
	}

	e2 := newEnv(t)
	p := good()
	p.CallbackBaseURL = "http://mpesa.example:8088/"
	if _, err := e2.svc.Save(ctx, actor, p); err != nil {
		t.Fatal(err)
	}
	if _, err := e2.svc.RegisterURLs(ctx, actor, ""); !errors.As(err, &ve) || ve.Field != "callback_base_url" {
		t.Fatalf("register with a refused base URL: %v", err)
	}
}

func TestGeneratedCallbackTokensAvoidRefusedWords(t *testing.T) {
	for range 2000 {
		tok, err := newCallbackToken()
		if err != nil {
			t.Fatal(err)
		}
		if w := mpesa.ForbiddenURLWord(tok.Reveal()); w != "" {
			t.Fatalf("token contains %q", w)
		}
	}
}
