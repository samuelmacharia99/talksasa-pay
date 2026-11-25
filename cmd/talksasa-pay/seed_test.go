package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/events"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/testutil/testdb"
)

// seed demo writes a consistent data set (the database guards accept every
// row) and is safe to re-run.
func TestSeedDemo(t *testing.T) {
	d := testdb.New(t)
	ctx := context.Background()
	kr, err := crypto.ParseKeyring("1:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), 0)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &services{pool: d.App, logger: logger, apps: app.NewService(app.Options{
		Pool: d.App, Keyring: kr, APIKeyPepper: []byte(strings.Repeat("p", 40)), Logger: logger,
	}), events: events.NewEmitter(apiview.View{PayBaseURL: "http://pay.test/"}, nil)}
	actor := audit.CLI("seed-test")

	var out bytes.Buffer
	if err := seedDemo(ctx, a, actor, &out); err != nil {
		t.Fatalf("seed: %v\n%s", err, out.String())
	}
	count := func(sql string) int {
		t.Helper()
		var n int
		if err := d.Owner.QueryRow(ctx, sql).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for sql, want := range map[string]int{
		`SELECT count(*) FROM apps`:                                       3,
		`SELECT count(*) FROM apps WHERE status = 'suspended'`:            1,
		`SELECT count(*) FROM charges`:                                    len(demoCharges),
		`SELECT count(*) FROM charges WHERE status = 'pending'`:           3,
		`SELECT count(*) FROM charges WHERE status = 'succeeded'`:         4,
		`SELECT count(*) FROM charges WHERE status = 'needs_review'`:      1,
		`SELECT count(*) FROM charges WHERE status = 'expired'`:           2,
		`SELECT count(*) FROM charges WHERE status = 'cancelled'`:         2,
		`SELECT count(*) FROM transactions WHERE app_id IS NULL`:          1,
		`SELECT count(*) FROM transactions WHERE status = 'needs_review'`: 1,
		`SELECT count(*) FROM transactions WHERE status = 'failed'`:       1,
		`SELECT count(*) FROM stk_pushes WHERE status = 'unknown'`:        1,
		// Every succeeded charge is paid by one succeeded, receipted
		// transaction for exactly its amount.
		`SELECT count(*) FROM charges c JOIN transactions t ON t.id = c.transaction_id
		  WHERE c.status = 'succeeded' AND t.status = 'succeeded' AND t.amount = c.amount
		    AND t.provider_transaction_id IS NOT NULL`: 4,
	} {
		if got := count(sql); got != want {
			t.Errorf("%s: %d, want %d", sql, got, want)
		}
	}

	out.Reset()
	if err := seedDemo(ctx, a, actor, &out); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if !strings.Contains(out.String(), "already seeded") || count(`SELECT count(*) FROM charges`) != len(demoCharges) {
		t.Fatalf("re-seed was not a no-op: %s", out.String())
	}
}
