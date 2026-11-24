-- Phase 2: M-Pesa (Daraja) STK pushes, callback inbox, pending transactions.

-- ---------------------------------------------------------------------------
-- Pending STK transactions
-- ---------------------------------------------------------------------------

-- name: InsertPendingSTKTransaction :one
-- The ledger row an STK push will complete. No receipt yet.
INSERT INTO transactions (id, product_id, direction, kind, channel, provider, status, amount,
                          phone, bill_ref, payer_account_id, payment_request_id)
VALUES (@id, @product_id, 'in', 'payment', 'mpesa_stk', 'mpesa', 'pending', @amount,
        @phone, @bill_ref, @payer_account_id, @payment_request_id)
RETURNING *;

-- name: SetTransactionProviderRequest :exec
-- Daraja accepted the push: remember its CheckoutRequestID (set once).
UPDATE transactions SET provider_request_id = @provider_request_id
 WHERE id = @id AND provider_request_id IS NULL;

-- name: GetTransactionByProviderRequest :one
SELECT * FROM transactions WHERE channel = @channel AND provider_request_id = @provider_request_id;

-- name: CompletePendingTransaction :one
-- pending | needs_review -> succeeded with Safaricom's receipt.
UPDATE transactions
   SET status = 'succeeded',
       provider_transaction_id = @receipt,
       phone = coalesce(phone, sqlc.narg(phone)),
       occurred_at = coalesce(occurred_at, sqlc.narg(occurred_at)),
       raw_payload = coalesce(raw_payload, sqlc.narg(raw_payload)),
       completed_at = now()
 WHERE id = @id AND status IN ('pending', 'needs_review')
RETURNING *;

-- name: FailPendingTransaction :one
-- pending | needs_review -> failed (customer cancelled, timeout, rejected...).
UPDATE transactions
   SET status = 'failed',
       failure_code = coalesce(failure_code, sqlc.narg(failure_code)),
       failure_reason = coalesce(failure_reason, sqlc.narg(failure_reason)),
       raw_payload = coalesce(raw_payload, sqlc.narg(raw_payload)),
       completed_at = now()
 WHERE id = @id AND status IN ('pending', 'needs_review')
RETURNING *;

-- name: MarkTransactionNeedsReview :one
UPDATE transactions
   SET status = 'needs_review',
       failure_code = coalesce(failure_code, sqlc.narg(failure_code)),
       failure_reason = coalesce(failure_reason, sqlc.narg(failure_reason))
 WHERE id = @id AND status = 'pending'
RETURNING *;

-- ---------------------------------------------------------------------------
-- STK pushes
-- ---------------------------------------------------------------------------

-- name: InsertSTKPush :one
INSERT INTO stk_pushes (id, product_id, payment_request_id, transaction_id, source, phone, amount,
                        idempotency_key, request_hash, next_query_at)
VALUES (@id, @product_id, @payment_request_id, @transaction_id, @source, @phone, @amount,
        sqlc.narg(idempotency_key), sqlc.narg(request_hash), @next_query_at)
RETURNING *;

-- name: GetSTKPushByIdempotencyKey :one
SELECT * FROM stk_pushes WHERE product_id = @product_id AND idempotency_key = @idempotency_key;

-- name: GetSTKPushForProduct :one
SELECT * FROM stk_pushes WHERE product_id = @product_id AND id = @id;

-- name: GetSTKPushInternal :one
SELECT * FROM stk_pushes WHERE id = @id;

-- name: LockSTKPush :one
SELECT * FROM stk_pushes WHERE id = @id FOR NO KEY UPDATE;

-- name: LockSTKPushByCheckout :one
SELECT * FROM stk_pushes WHERE checkout_request_id = @checkout_request_id FOR NO KEY UPDATE;

-- name: GetLiveSTKPushForRequest :one
SELECT * FROM stk_pushes WHERE payment_request_id = @payment_request_id AND status IN ('initiating', 'sent');

-- name: ListSTKPushesForRequest :many
SELECT * FROM stk_pushes WHERE payment_request_id = @payment_request_id ORDER BY created_at DESC LIMIT @lim;

-- name: MarkSTKPushSent :one
UPDATE stk_pushes
   SET status = 'sent', merchant_request_id = @merchant_request_id,
       checkout_request_id = @checkout_request_id, next_query_at = @next_query_at
 WHERE id = @id AND status = 'initiating'
RETURNING *;

-- name: FinishSTKPush :one
-- Moves a push to a final (or unknown) state; the guard trigger enforces
-- which transitions are allowed.
UPDATE stk_pushes
   SET status = @status, result_code = sqlc.narg(result_code), result_desc = sqlc.narg(result_desc),
       completed_at = coalesce(completed_at, now()), next_query_at = NULL
 WHERE id = @id
RETURNING *;

-- name: ScheduleSTKQuery :exec
UPDATE stk_pushes
   SET query_attempts = query_attempts + 1, next_query_at = @next_query_at,
       result_code = coalesce(sqlc.narg(result_code), result_code),
       result_desc = coalesce(sqlc.narg(result_desc), result_desc)
 WHERE id = @id;

-- name: ListDueSTKPushes :many
-- Status-check sweeper. SKIP LOCKED: several workers never check the same
-- push at once, and a push being completed by a callback is skipped.
SELECT * FROM stk_pushes
 WHERE status IN ('initiating', 'sent') AND next_query_at <= now()
 ORDER BY next_query_at
 LIMIT @lim
   FOR NO KEY UPDATE SKIP LOCKED;

-- name: FindOrphanSTKPush :many
-- A success callback whose CheckoutRequestID we never stored (crash between
-- asking Daraja and saving its answer): candidates by phone and amount.
SELECT * FROM stk_pushes
 WHERE checkout_request_id IS NULL AND phone = @phone AND amount = @amount
   AND created_at > now() - interval '1 hour'
 ORDER BY created_at DESC
 LIMIT 2;

-- ---------------------------------------------------------------------------
-- Callback inbox
-- ---------------------------------------------------------------------------

-- name: InsertMpesaCallback :one
INSERT INTO mpesa_callbacks (id, kind, remote_ip, raw)
VALUES (@id, @kind, sqlc.narg(remote_ip), @raw)
RETURNING *;

-- name: LockMpesaCallback :one
SELECT * FROM mpesa_callbacks WHERE id = @id FOR NO KEY UPDATE;

-- name: MarkMpesaCallbackProcessed :exec
UPDATE mpesa_callbacks
   SET processed_at = now(), error = sqlc.narg(error), attempts = attempts + 1,
       transaction_id = coalesce(transaction_id, sqlc.narg(transaction_id))
 WHERE id = @id AND processed_at IS NULL;

-- name: RecordMpesaCallbackError :exec
UPDATE mpesa_callbacks SET error = @error, attempts = attempts + 1
 WHERE id = @id AND processed_at IS NULL;

-- name: ListMpesaCallbacksAdmin :many
SELECT id, kind, remote_ip, received_at, processed_at, error, attempts, transaction_id
  FROM mpesa_callbacks
 WHERE (NOT @unprocessed_only::boolean OR processed_at IS NULL)
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (received_at, id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY received_at DESC, id DESC
 LIMIT @lim;
