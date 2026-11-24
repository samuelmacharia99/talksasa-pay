package integrationtest

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/store"
)

func stormPhone(i int) string { return fmt.Sprintf("07%08d", 10000000+i) } // 07 1xxxxxxx

// 9a. Many distinct payers created at once get unique, well-formed,
// never-duplicated account numbers (gaps are fine) that route back to them.
func TestConcurrencyDistinctPayersUniqueAccountNumbers(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("ACCT").Product

	const n = 200
	got := make([]payer.Payer, n)
	errs := storm(n, func(i int) error {
		var pr payer.Payer
		var created bool
		err := retryAuditFork(func() error {
			var err error
			pr, created, err = e.payers.Upsert(e.ctx, operator, p.ID, payer.UpsertParams{
				ExternalRef: fmt.Sprintf("cust-%d", i), DisplayName: "Customer", Phones: []string{stormPhone(i)},
			})
			return err
		})
		if err == nil && !created {
			err = fmt.Errorf("payer %d not reported as created", i)
		}
		got[i] = pr
		return err
	})
	e.skipIfAuditWedged(errs...)
	requireNoErrors(t, "upsert", errs)

	seen := map[string]bool{}
	for i, pr := range got {
		if !strings.HasPrefix(pr.AccountNumber, "ACCT-") || seen[pr.AccountNumber] {
			t.Fatalf("payer %d: account number %q (duplicate=%v)", i, pr.AccountNumber, seen[pr.AccountNumber])
		}
		seen[pr.AccountNumber] = true
		rr, ok, err := e.payers.Route(e.ctx, store.New(e.db.App), pr.AccountNumber)
		if err != nil || !ok || rr.PayerID != pr.ID {
			t.Fatalf("payer %d: %s routes to %+v ok=%v err=%v", i, pr.AccountNumber, rr, ok, err)
		}
	}
	if c := e.count(`SELECT count(DISTINCT account_number) FROM payer_accounts WHERE product_id = $1`, p.ID); c != n {
		t.Fatalf("distinct account numbers = %d", c)
	}
	t.Logf("audit-chain append races retried so far: %d", auditForks.Load())
	e.checkInvariants(invariantOpts{})
}

// 9b. 100 concurrent upserts of the SAME external_ref with different phones:
// one payer, one account number, and the stored phone set equals exactly one
// caller's input (last writer wins; never a mix of two callers' phones).
func TestConcurrencyPayerUpsertStorm(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("UPS").Product

	const n = 100
	ids := make([]uuid.UUID, n)
	accts := make([]string, n)
	var created atomic.Int64
	errs := storm(n, func(i int) error {
		pr, c, err := e.payers.Upsert(e.ctx, operator, p.ID, payer.UpsertParams{
			ExternalRef: "same-ref", DisplayName: "Customer", Email: "c@example.com",
			Phones: []string{stormPhone(i)},
		})
		if c {
			created.Add(1)
		}
		ids[i], accts[i] = pr.ID, pr.AccountNumber
		return err
	})
	failed := 0
	var firstErr error
	for _, err := range errs {
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	// The identity part must hold regardless.
	if c := e.count(`SELECT count(*) FROM payer_accounts WHERE product_id = $1`, p.ID); c != 1 {
		t.Fatalf("payers = %d", c)
	}
	if created.Load() != 1 {
		t.Fatalf("created reported %d times", created.Load())
	}
	var winner uuid.UUID
	var winnerAcct string
	for i := range ids {
		if errs[i] != nil {
			continue
		}
		if winner == uuid.Nil {
			winner, winnerAcct = ids[i], accts[i]
		}
		if ids[i] != winner || accts[i] != winnerAcct {
			t.Fatalf("caller %d got payer %s/%s, others %s/%s", i, ids[i], accts[i], winner, winnerAcct)
		}
	}

	var phones []string
	rows, err := e.db.Owner.Query(e.ctx, `SELECT phone FROM payer_phones pp JOIN payer_accounts pa ON pa.id = pp.payer_account_id
		WHERE pa.product_id = $1 ORDER BY phone`, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ph string
		if err := rows.Scan(&ph); err != nil {
			t.Fatal(err)
		}
		phones = append(phones, ph)
	}
	rows.Close()
	primaries := e.count(`SELECT count(*) FROM payer_phones pp JOIN payer_accounts pa ON pa.id = pp.payer_account_id
		WHERE pa.product_id = $1 AND pp.is_primary`, p.ID)
	t.Logf("upsert storm: %d/%d failed (first: %v); final phones %v, primaries %d", failed, n, firstErr, phones, primaries)

	if failed > 0 || len(phones) != 1 || primaries != 1 {
		e.checkInvariants(invariantOpts{}) // money/audit are still fine either way
		t.Fatalf("upsert storm: %d failed (first %v); phones %v; primaries %d — want 0 failures, exactly one caller's phone, one primary",
			failed, firstErr, phones, primaries)
	}
	want := map[string]bool{}
	for i := 0; i < n; i++ {
		want["254"+stormPhone(i)[1:]] = true
	}
	if !want[phones[0]] {
		t.Fatalf("stored phone %s is not one of the inputs", phones[0])
	}
	e.checkInvariants(invariantOpts{})
}
