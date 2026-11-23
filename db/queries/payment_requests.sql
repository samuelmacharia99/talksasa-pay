-- name: CreatePaymentRequest :one
INSERT INTO payment_requests (id, product_id, payer_account_id, product_reference, description, amount,
                              expires_at, hosted_token, idempotency_key)
VALUES (@id, @product_id, @payer_account_id, @product_reference, @description, @amount,
        @expires_at, @hosted_token, @idempotency_key)
RETURNING *;

-- name: GetPaymentRequest :one
SELECT * FROM payment_requests WHERE product_id = @product_id AND id = @id;

-- name: GetPaymentRequestByIdempotencyKey :one
SELECT * FROM payment_requests WHERE product_id = @product_id AND idempotency_key = @idempotency_key;

-- name: LockPaymentRequest :one
SELECT * FROM payment_requests WHERE product_id = @product_id AND id = @id FOR NO KEY UPDATE;

-- name: ListPayableRequestsForUpdate :many
-- Called after LockPayer. Oldest first, matching the allocation engine.
SELECT * FROM payment_requests
 WHERE product_id = @product_id AND payer_account_id = @payer_account_id
   AND status IN ('open', 'partially_paid')
 ORDER BY created_at, id
   FOR NO KEY UPDATE;

-- name: ListPaymentRequestsByIDs :many
SELECT * FROM payment_requests WHERE product_id = @product_id AND id = ANY(@ids::uuid[]) ORDER BY created_at, id;

-- name: CancelPaymentRequest :one
UPDATE payment_requests
   SET status = 'cancelled', closed_at = now()
 WHERE product_id = @product_id AND id = @id AND status = 'open'
RETURNING *;

-- name: ExpireDuePaymentRequests :many
-- Only untouched (open) requests expire; partially paid ones stay payable.
UPDATE payment_requests
   SET status = 'expired', closed_at = now()
 WHERE id IN (
   SELECT pr.id FROM payment_requests pr
    WHERE pr.status = 'open' AND pr.expires_at IS NOT NULL AND pr.expires_at <= now()
    ORDER BY pr.expires_at
    LIMIT @batch_size
    FOR NO KEY UPDATE SKIP LOCKED)
RETURNING *;
