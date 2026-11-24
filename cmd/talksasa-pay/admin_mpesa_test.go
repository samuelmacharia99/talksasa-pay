package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

const testToken = "s3cr3t-callback-token_ABC"

type fakeRegistrar struct {
	got mpesa.RegisterURLsRequest
	err error
}

func (f *fakeRegistrar) RegisterC2BURLs(_ context.Context, req mpesa.RegisterURLsRequest) error {
	f.got = req
	return f.err
}

func (f *fakeRegistrar) CallbackURLs() (string, string, string) {
	base := "https://api.pay.example/cb/mpesa/" + testToken
	return base + "/stk", base + "/c2b/confirmation", base + "/c2b/validation"
}

func TestRedactToken(t *testing.T) {
	if got := redactToken("https://x/cb/mpesa/"+testToken+"/stk", testToken); got != "https://x/cb/mpesa/[TOKEN]/stk" {
		t.Fatal(got)
	}
	if got := redactToken("https://x/cb/mpesa/a%2Fb/stk", "a/b"); strings.Contains(got, "a%2Fb") {
		t.Fatal(got)
	}
	if got := redactToken("https://x/", ""); got != "https://x/" {
		t.Fatal(got)
	}
}

func TestRegisterC2BURLs(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	actor := audit.CLI("Jane Ops")
	var out bytes.Buffer

	f := &fakeRegistrar{}
	if err := registerC2BURLs(ctx, f, d.App, actor, "", testToken, &out); err == nil {
		t.Fatal("registered without a paybill")
	}

	if err := registerC2BURLs(ctx, f, d.App, actor, "4001234", testToken, &out); err != nil {
		t.Fatal(err)
	}
	_, conf, val := f.CallbackURLs()
	if f.got != (mpesa.RegisterURLsRequest{ShortCode: "4001234", ConfirmationURL: conf, ValidationURL: val, ResponseType: "Completed"}) {
		t.Fatalf("request %+v", f.got)
	}
	if strings.Contains(out.String(), testToken) || !strings.Contains(out.String(), "/cb/mpesa/[TOKEN]/c2b/confirmation") {
		t.Fatalf("output leaks or lacks URLs:\n%s", out.String())
	}

	var action, actorID, details string
	if err := d.Owner.QueryRow(ctx, `SELECT action, actor_id, details::text FROM audit_log ORDER BY id DESC LIMIT 1`).
		Scan(&action, &actorID, &details); err != nil {
		t.Fatal(err)
	}
	if action != "mpesa.paybill_urls_registered" || actorID != "Jane Ops" || strings.Contains(details, testToken) ||
		!strings.Contains(details, "[TOKEN]") || !strings.Contains(details, "4001234") {
		t.Fatalf("audit row: %s %s %s", action, actorID, details)
	}

	// A Daraja error that echoes the URL never leaks the token, and nothing
	// is audited.
	f.err = errors.New("mpesa: rejected: invalid ConfirmationURL " + conf)
	before := countAudit(t, d)
	err := registerC2BURLs(ctx, f, d.App, actor, "4001234", testToken, &out)
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("error %v", err)
	}
	if countAudit(t, d) != before {
		t.Fatal("failed registration was audited as done")
	}
}

func countAudit(t *testing.T, d *testdb.DB) int {
	t.Helper()
	var n int
	if err := d.Owner.QueryRow(context.Background(), `SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPrintMpesaStatus(t *testing.T) {
	d := testdb.New(t)
	var out bytes.Buffer
	cfg := &config.Config{}
	cfg.Mpesa = config.MpesaConfig{Env: config.MpesaSandbox, ShortCode: "174379", CallbackBaseURL: "https://api.pay.example/",
		CallbackToken: config.NewSecret(testToken)}
	if err := printMpesaStatus(context.Background(), cfg, store.New(d.App), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"sandbox", "174379", "https://api.pay.example/", "Unknown STK pushes (staff review):     0", "Unprocessed callbacks:                 0"} {
		if !strings.Contains(s, want) {
			t.Errorf("status lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, testToken) {
		t.Fatal("status printed the token")
	}
	out.Reset()
	if err := printMpesaStatus(context.Background(), &config.Config{}, store.New(d.App), &out); err != nil || !strings.Contains(out.String(), "off") {
		t.Fatalf("off: %v %s", err, out.String())
	}
}
