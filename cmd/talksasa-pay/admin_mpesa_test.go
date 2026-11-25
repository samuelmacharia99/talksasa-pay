package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/mpesasettings"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

type fakeRegistrar struct {
	confirm string
	err     error
}

func (f *fakeRegistrar) RegisterURLs(_ context.Context, _ audit.Actor, confirm string) (mpesasettings.RegisterResult, error) {
	f.confirm = confirm
	if f.err != nil {
		return mpesasettings.RegisterResult{}, f.err
	}
	return mpesasettings.RegisterResult{At: time.Now(), ShortCode: "4001234", ResponseType: "Completed",
		ConfirmationURL: "https://api.pay.example/cb/mpesa/[TOKEN]/c2b/confirmation",
		ValidationURL:   "https://api.pay.example/cb/mpesa/[TOKEN]/c2b/validation"}, nil
}

// The CLI uses the same service as the admin panel; registration itself
// (Daraja, records, audit, redaction) is tested in internal/mpesasettings.
func TestRegisterC2BURLs(t *testing.T) {
	var out bytes.Buffer
	f := &fakeRegistrar{}
	if err := registerC2BURLs(context.Background(), f, audit.CLI("Jane Ops"), "production", &out); err != nil {
		t.Fatal(err)
	}
	if f.confirm != "production" || !strings.Contains(out.String(), "/cb/mpesa/[TOKEN]/c2b/confirmation") || !strings.Contains(out.String(), "4001234") {
		t.Fatalf("confirm %q, output:\n%s", f.confirm, out.String())
	}
	f.err = errors.New("boom")
	if err := registerC2BURLs(context.Background(), f, audit.CLI("Jane Ops"), "", &out); err == nil {
		t.Fatal("error swallowed")
	}
}

func TestPrintMpesaStatus(t *testing.T) {
	d := testdb.New(t)
	var out bytes.Buffer
	st := mpesasettings.Status{Source: mpesasettings.SourceDatabase, Environment: "sandbox", Paybill: "174379",
		CallbackBaseURL: "https://api.pay.example/", Ready: true}
	if err := printMpesaStatus(context.Background(), st, store.New(d.App), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"sandbox", "admin panel", "174379", "https://api.pay.example/", "Unknown STK pushes (staff review):     0", "Unprocessed callbacks:                 0"} {
		if !strings.Contains(s, want) {
			t.Errorf("status lacks %q:\n%s", want, s)
		}
	}
	out.Reset()
	if err := printMpesaStatus(context.Background(), mpesasettings.Status{Source: mpesasettings.SourceNone}, store.New(d.App), &out); err != nil ||
		!strings.Contains(out.String(), "off") {
		t.Fatalf("off: %v %s", err, out.String())
	}
}
