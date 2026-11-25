-- name: InsertTransaction :one
-- Returns no row (pgx.ErrNoRows) when the receipt is already recorded.
INSERT INTO transactions (id, app_id, charge_id, direction, kind, channel, provider, status, amount,
                          provider_transaction_id, provider_request_id, phone, account_number,
                          reverses_transaction_id, failure_code, failure_reason, reason, created_by,
                          raw_payload, occurred_at, completed_at)
VALUES (@id, @app_id, @charge_id, @direction, @kind, @channel, @provider, @status, @amount,
        @provider_transaction_id, @provider_request_id, @phone, @account_number,
        @reverses_transaction_id, @failure_code, @failure_reason, @reason, @created_by,
        @raw_payload, @occurred_at, @completed_at)
ON CONFLICT ON CONSTRAINT transactions_provider_receipt_uq DO NOTHING
RETURNING *;

-- name: GetTransactionByReceipt :one
SELECT * FROM transactions WHERE provider = @provider AND provider_transaction_id = @provider_transaction_id;

-- name: GetTransactionInternal :one
-- Internal use only (workers, admin): not app-scoped because unmatched
-- payments have no app yet.
SELECT * FROM transactions WHERE id = @id;

-- name: LockTransactionInternal :one
SELECT * FROM transactions WHERE id = @id FOR NO KEY UPDATE;

-- name: GetTransactionForApp :one
SELECT * FROM transactions WHERE app_id = @app_id AND id = @id;

-- name: GetReversalOf :one
SELECT * FROM transactions WHERE kind = 'reversal' AND reverses_transaction_id = @original_id;

-- name: AssignTransactionApp :one
-- Staff assign an unmatched payment to an app (set once; the guard trigger
-- refuses a change). No row when it already has an app.
UPDATE transactions SET app_id = @app_id
 WHERE id = @id AND app_id IS NULL
RETURNING *;
