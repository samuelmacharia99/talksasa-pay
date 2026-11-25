package integrationtest

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// execFails asserts a statement is rejected and the error mentions want.
func execFails(t *testing.T, pool *pgxpool.Pool, want, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(context.Background(), sql, args...)
	if err == nil {
		t.Fatalf("expected failure for %q", sql)
	}
	if want != "" && !strings.Contains(err.Error(), want) {
		t.Fatalf("%q: error %q does not mention %q", sql, err, want)
	}
}

func TestLedgerIsAppendOnlyForAppRole(t *testing.T) {
	e := newEnv(t)
	p := e.app("HOST").App
	ch := e.charge(p.ID, 1000)
	txn := e.payCharge(ch)
	app := e.db.App

	// No DELETE privilege at all for the application role.
	execFails(t, app, "permission denied", `DELETE FROM transactions WHERE id = $1`, txn.ID)
	execFails(t, app, "permission denied", `DELETE FROM charges WHERE id = $1`, ch.ID)
	execFails(t, app, "permission denied", `DELETE FROM audit_log`)
	execFails(t, app, "permission denied", `TRUNCATE transactions`)

	// Money columns cannot be changed.
	execFails(t, app, "immutable", `UPDATE transactions SET amount = 1 WHERE id = $1`, txn.ID)
	execFails(t, app, "cannot be changed", `UPDATE transactions SET provider_transaction_id = 'X' WHERE id = $1`, txn.ID)
	execFails(t, app, "not allowed", `UPDATE transactions SET status = 'pending' WHERE id = $1`, txn.ID)
	execFails(t, app, "immutable", `UPDATE charges SET amount = 1 WHERE id = $1`, ch.ID)
	execFails(t, app, "immutable", `UPDATE charges SET reference = 'other' WHERE id = $1`, ch.ID)
	execFails(t, app, "immutable", `UPDATE charges SET account_number = 'HOST-1', account_number_norm = 'HOST1' WHERE id = $1`, ch.ID)
	execFails(t, app, "set once", `UPDATE charges SET transaction_id = NULL, succeeded_at = NULL, status = 'pending' WHERE id = $1`, ch.ID)
	execFails(t, app, "not allowed", `UPDATE charges SET status = 'pending' WHERE id = $1`, ch.ID)
}

// A charge succeeds only by pointing at a succeeded, receipted payment of the
// same app and charge for exactly its amount (charges_guard).
func TestChargeSucceedsOnlyWithItsExactPayment(t *testing.T) {
	e := newEnv(t)
	p := e.app("HOST").App
	paid := e.charge(p.ID, 1000)
	txn := e.payCharge(paid)
	other := e.charge(p.ID, 1000)
	small := e.charge(p.ID, 999)

	for _, c := range []struct {
		name   string
		charge uuid.UUID
		txn    uuid.UUID
	}{
		{"another charge's payment", other.ID, txn.ID},
		{"wrong amount", small.ID, txn.ID},
		{"no transaction", other.ID, uuid.Must(uuid.NewV7())},
	} {
		_, err := e.db.App.Exec(e.ctx, `UPDATE charges SET status = 'succeeded', transaction_id = $2, succeeded_at = now() WHERE id = $1`, c.charge, c.txn)
		if err == nil {
			t.Fatalf("%s: charge succeeded", c.name)
		}
	}
	// A pending (unreceipted) STK payment cannot succeed a charge either.
	if _, err := e.db.App.Exec(e.ctx, `INSERT INTO transactions (id, app_id, charge_id, direction, kind, channel, provider, status, amount)
		VALUES ($1, $2, $3, 'in', 'payment', 'mpesa_stk', 'mpesa', 'pending', 1000)`, uuid.Must(uuid.NewV7()), p.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	execFails(t, e.db.App, "can only be paid", `UPDATE charges SET status = 'succeeded', succeeded_at = now(),
		transaction_id = (SELECT id FROM transactions WHERE charge_id = $1 AND status = 'pending') WHERE id = $1`, other.ID)

	// A reversal must undo a succeeded payment of the same app and charge,
	// for its full amount; a charge is reversed only after that.
	execFails(t, e.db.App, "reversed only after", `UPDATE charges SET status = 'reversed' WHERE id = $1`, paid.ID)
	execFails(t, e.db.App, "full amount", `INSERT INTO transactions (id, app_id, charge_id, direction, kind, channel, provider, status,
		amount, reverses_transaction_id, reason, created_by, completed_at)
		VALUES ($1, $2, $3, 'out', 'reversal', 'mpesa_c2b', 'mpesa', 'succeeded', 999, $4, 'customer refund', 'admin:x', now())`,
		uuid.Must(uuid.NewV7()), p.ID, paid.ID, txn.ID)
}

func TestLedgerIsAppendOnlyEvenForOwner(t *testing.T) {
	e := newEnv(t)
	p := e.app("HOST").App
	ch := e.charge(p.ID, 500)
	txn := e.payCharge(ch)
	owner := e.db.Owner

	execFails(t, owner, "cannot be deleted", `DELETE FROM transactions WHERE id = $1`, txn.ID)
	execFails(t, owner, "cannot be truncated", `TRUNCATE transactions CASCADE`)
	execFails(t, owner, "immutable", `UPDATE transactions SET amount = 1 WHERE id = $1`, txn.ID)
	execFails(t, owner, "cannot be deleted", `DELETE FROM charges`)
	execFails(t, owner, "immutable", `UPDATE charges SET amount = 1 WHERE id = $1`, ch.ID)
	execFails(t, owner, "append-only", `UPDATE audit_log SET actor_id = 'someone-else'`)
	execFails(t, owner, "cannot be deleted", `DELETE FROM apps`)
}

func TestAuditChainDetectsTampering(t *testing.T) {
	e := newEnv(t)
	e.app("HOST")
	e.app("BILL")
	e.assertAuditChain() // chain verifies

	// Simulate a superuser editing history behind the triggers' back.
	if _, err := e.db.Owner.Exec(e.ctx, `ALTER TABLE audit_log DISABLE TRIGGER audit_log_no_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Owner.Exec(e.ctx, `UPDATE audit_log SET actor_id = 'nobody' WHERE id = (SELECT min(id) FROM audit_log)`); err != nil {
		t.Fatal(err)
	}
	var bad int64
	if err := e.db.App.QueryRow(e.ctx, `SELECT coalesce(tsp_audit_verify(), 0)`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad == 0 {
		t.Fatal("tampering not detected")
	}
}

func TestTransactionEventsRecordActor(t *testing.T) {
	e := newEnv(t)
	p := e.app("HOST").App
	txn, err := e.paymentErr(e.ctx, &p.ID, nil, 100, newReceipt())
	if err != nil {
		t.Fatal(err)
	}
	var actor, to string
	if err := e.db.App.QueryRow(e.ctx, `SELECT actor, to_status::text FROM transaction_events WHERE transaction_id = $1`, txn.ID).Scan(&actor, &to); err != nil {
		t.Fatal(err)
	}
	if actor != worker.String() || to != "succeeded" {
		t.Fatalf("event actor=%q to=%q", actor, to)
	}
}

func TestSecretsNotStoredInPlaintext(t *testing.T) {
	e := newEnv(t)
	c := e.app("HOST")
	k, err := e.apps.IssueAPIKey(e.ctx, operator, c.App.ID, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	secretPart := k.Plaintext[strings.LastIndex(k.Plaintext, "_")+1:]

	var dump string
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT
		(SELECT string_agg(encode(webhook_secret_ciphertext, 'escape'), '') FROM apps) ||
		(SELECT string_agg(row_to_json(k)::text, '') FROM app_api_keys k) ||
		(SELECT string_agg(row_to_json(a)::text, '') FROM audit_log a)`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, c.WebhookSecret) || strings.Contains(dump, secretPart) {
		t.Fatal("a secret was found in plaintext in the database")
	}
	if strings.Contains(e.logs.String(), c.WebhookSecret) || strings.Contains(e.logs.String(), secretPart) {
		t.Fatal("a secret was found in the logs")
	}
	got, err := e.apps.WebhookSecret(e.ctx, c.App.ID)
	if err != nil || string(got) != c.WebhookSecret {
		t.Fatalf("decrypt webhook secret: %v", err)
	}
}
