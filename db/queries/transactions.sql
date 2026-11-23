-- name: InsertTransaction :one
-- Returns no row (pgx.ErrNoRows) when the receipt is already recorded.
INSERT INTO transactions (id, product_id, direction, kind, channel, provider, status, amount,
                          provider_transaction_id, provider_request_id, phone, bill_ref,
                          payer_account_id, payment_request_id, reverses_transaction_id,
                          failure_code, failure_reason, reason, created_by, raw_payload,
                          occurred_at, completed_at)
VALUES (@id, @product_id, @direction, @kind, @channel, @provider, @status, @amount,
        @provider_transaction_id, @provider_request_id, @phone, @bill_ref,
        @payer_account_id, @payment_request_id, @reverses_transaction_id,
        @failure_code, @failure_reason, @reason, @created_by, @raw_payload,
        @occurred_at, @completed_at)
ON CONFLICT ON CONSTRAINT transactions_provider_receipt_uq DO NOTHING
RETURNING *;

-- name: GetTransactionByReceipt :one
SELECT * FROM transactions WHERE provider = @provider AND provider_transaction_id = @provider_transaction_id;

-- name: GetTransactionInternal :one
-- Internal use only (workers, admin): not product-scoped because unmatched
-- payments have no product yet.
SELECT * FROM transactions WHERE id = @id;

-- name: LockTransactionInternal :one
SELECT * FROM transactions WHERE id = @id FOR NO KEY UPDATE;

-- name: GetTransactionForProduct :one
SELECT * FROM transactions WHERE product_id = @product_id AND id = @id;

-- name: GetReversalOf :one
SELECT * FROM transactions WHERE kind = 'reversal' AND reverses_transaction_id = @original_id;
