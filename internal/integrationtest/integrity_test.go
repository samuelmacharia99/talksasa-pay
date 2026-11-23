package integrationtest

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	r := e.request(p.ID, pr.ID, 1000)
	txn, _ := e.pay(pr, 1500, nil, newReceipt())
	app := e.db.App

	// No DELETE privilege at all for the application role.
	execFails(t, app, "permission denied", `DELETE FROM transactions WHERE id = $1`, txn.ID)
	execFails(t, app, "permission denied", `DELETE FROM allocations`)
	execFails(t, app, "permission denied", `DELETE FROM audit_log`)
	execFails(t, app, "permission denied", `TRUNCATE transactions`)

	// Money columns cannot be changed.
	execFails(t, app, "immutable", `UPDATE transactions SET amount = 1 WHERE id = $1`, txn.ID)
	execFails(t, app, "cannot be changed", `UPDATE transactions SET provider_transaction_id = 'X' WHERE id = $1`, txn.ID)
	execFails(t, app, "not allowed", `UPDATE transactions SET status = 'pending' WHERE id = $1`, txn.ID)
	execFails(t, app, "permission denied", `UPDATE allocations SET amount = 1`)
	execFails(t, app, "immutable", `UPDATE payment_requests SET amount = 1 WHERE id = $1`, r.ID)
	execFails(t, app, "only change through allocations", `UPDATE payment_requests SET amount_paid = 0 WHERE id = $1`, r.ID)
	execFails(t, app, "only change through allocations", `UPDATE payer_accounts SET credit_balance = 999999 WHERE id = $1`, pr.ID)
	execFails(t, app, "immutable", `UPDATE payer_accounts SET account_number = 'HOST-1' WHERE id = $1`, pr.ID)

	// Setting the internal flag by hand does not bypass the guard: the
	// trigger-depth check cannot be faked from a client session.
	err := pgx.BeginFunc(e.ctx, app, func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `SELECT set_config('talksasa.allocating', 'on', true)`); err != nil {
			return err
		}
		_, err := tx.Exec(e.ctx, `UPDATE payer_accounts SET credit_balance = 999999 WHERE id = $1`, pr.ID)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "only change through allocations") {
		t.Fatalf("guard bypassed with GUC: %v", err)
	}
}

func TestLedgerIsAppendOnlyEvenForOwner(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	txn, _ := e.pay(pr, 500, nil, newReceipt())
	owner := e.db.Owner

	execFails(t, owner, "cannot be deleted", `DELETE FROM transactions WHERE id = $1`, txn.ID)
	execFails(t, owner, "cannot be truncated", `TRUNCATE transactions CASCADE`)
	execFails(t, owner, "immutable", `UPDATE transactions SET amount = 1 WHERE id = $1`, txn.ID)
	execFails(t, owner, "cannot be deleted", `DELETE FROM allocations`)
	execFails(t, owner, "append-only", `UPDATE allocation_batches SET expected_total = 1`)
	execFails(t, owner, "append-only", `UPDATE audit_log SET actor_id = 'someone-else'`)
	execFails(t, owner, "cannot be deleted", `DELETE FROM payer_accounts`)
}

func TestUnbalancedBatchRejectedAtCommit(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	// Pay 1000 against a 300 request (700 becomes credit), then open a 5000
	// request that auto-consumes the 700. Finally try to write a credit
	// application whose lines do not net to zero: money from nothing.
	e.request(p.ID, pr.ID, 300)
	e.pay(pr, 1000, nil, newReceipt())
	r := e.request(p.ID, pr.ID, 5000)

	err := pgx.BeginFunc(e.ctx, e.db.App, func(tx pgx.Tx) error {
		b := uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(e.ctx, `INSERT INTO allocation_batches (id, product_id, payer_account_id, kind, payment_request_id, expected_total, created_by)
			VALUES ($1, $2, $3, 'credit_application', $4, 0, 'attacker')`, b, p.ID, pr.ID, r.ID); err != nil {
			return err
		}
		_, err := tx.Exec(e.ctx, `INSERT INTO allocations (batch_id, line_no, product_id, payer_account_id, payment_request_id, amount)
			VALUES ($1, 1, $2, $3, $4, 100)`, b, p.ID, pr.ID, r.ID)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "unbalanced") {
		t.Fatalf("unbalanced batch committed: %v", err)
	}
	assertStatus(t, e.reqState(p.ID, r.ID), "partially_paid", 700)
	e.assertNoDrift()
}

func TestPaymentBatchMustMatchTransactionAmount(t *testing.T) {
	e := newEnv(t)
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	// Record without allocating: insert an unmatched-then-matched style txn directly.
	txnID := uuid.Must(uuid.NewV7())
	if _, err := e.db.App.Exec(e.ctx, `INSERT INTO transactions (id, product_id, direction, kind, channel, provider, status, amount,
		provider_transaction_id, payer_account_id, completed_at) VALUES ($1,$2,'in','payment','mpesa_c2b','mpesa','succeeded',100,'RCPT1',$3,now())`,
		txnID, p.ID, pr.ID); err != nil {
		t.Fatal(err)
	}
	execFails(t, e.db.App, "exactly the incoming payment amount",
		`INSERT INTO allocation_batches (id, product_id, payer_account_id, kind, transaction_id, expected_total, created_by)
		 VALUES ($1, $2, $3, 'payment', $4, 1000000, 'attacker')`, uuid.Must(uuid.NewV7()), p.ID, pr.ID, txnID)
}

func TestAuditChainDetectsTampering(t *testing.T) {
	e := newEnv(t)
	e.product("HOST")
	e.product("BILL")
	e.assertNoDrift() // chain verifies

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
	p := e.product("HOST").Product
	pr := e.payer(p.ID, "c1")
	txn, _ := e.pay(pr, 100, nil, newReceipt())
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
	c := e.product("HOST")
	k, err := e.products.IssueAPIKey(e.ctx, operator, c.Product.ID, "ci", nil)
	if err != nil {
		t.Fatal(err)
	}
	secretPart := k.Plaintext[strings.LastIndex(k.Plaintext, "_")+1:]

	var dump string
	if err := e.db.Owner.QueryRow(e.ctx, `SELECT
		(SELECT string_agg(encode(webhook_secret_ciphertext, 'escape'), '') FROM products) ||
		(SELECT string_agg(row_to_json(k)::text, '') FROM product_api_keys k) ||
		(SELECT string_agg(row_to_json(a)::text, '') FROM audit_log a)`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dump, c.WebhookSecret) || strings.Contains(dump, secretPart) {
		t.Fatal("a secret was found in plaintext in the database")
	}
	if strings.Contains(e.logs.String(), c.WebhookSecret) || strings.Contains(e.logs.String(), secretPart) {
		t.Fatal("a secret was found in the logs")
	}
	got, err := e.products.WebhookSecret(e.ctx, c.Product.ID)
	if err != nil || string(got) != c.WebhookSecret {
		t.Fatalf("decrypt webhook secret: %v", err)
	}
}
