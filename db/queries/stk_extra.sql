-- Phase 2: extra queries for internal/stk (STK push service).

-- name: ListDueSTKPushesAt :many
-- Status-check sweeper, with the service clock instead of now() so the
-- schedule written by the service (next_query_at) and the claim agree.
-- SKIP LOCKED: several workers never check the same push at once, and a push
-- being completed by a callback is skipped.
SELECT * FROM stk_pushes
 WHERE status IN ('initiating', 'sent') AND next_query_at <= @now::timestamptz
 ORDER BY next_query_at
 LIMIT @lim
   FOR NO KEY UPDATE SKIP LOCKED;

-- name: LeaseSTKPush :exec
-- Claim a due push for one sweep: push its next check out by a lease so no
-- other sweeper takes it while we talk to Daraja. Does not count as a query.
UPDATE stk_pushes SET next_query_at = @next_query_at
 WHERE id = @id AND status IN ('initiating', 'sent');

-- name: FindOrphanSTKPushes :many
-- A success callback whose CheckoutRequestID we never stored (crash or
-- transient error between asking Daraja and saving its answer). Candidates:
-- pushes without a checkout id, by phone and amount, still initiating or
-- failed because we never heard back ("not_sent"). Pushes Daraja definitely
-- rejected (no prompt was ever shown) are excluded. The caller picks a
-- single candidate (see pushForSuccess) or gives up.
SELECT p.* FROM stk_pushes p
  JOIN transactions t ON t.id = p.transaction_id
 WHERE p.checkout_request_id IS NULL AND p.phone = @phone AND p.amount = @amount
   AND p.status IN ('initiating', 'failed')
   AND (p.status = 'initiating' OR t.failure_code IS DISTINCT FROM 'rejected')
   AND p.created_at > now() - interval '1 hour'
 ORDER BY p.created_at DESC
 LIMIT 10;

-- name: SetSTKPushCheckout :one
-- Attach Safaricom's ids to a push that never stored them (orphan match of a
-- push we had already failed). Set once.
UPDATE stk_pushes
   SET merchant_request_id = coalesce(merchant_request_id, sqlc.narg(merchant_request_id)),
       checkout_request_id = @checkout_request_id
 WHERE id = @id AND checkout_request_id IS NULL
RETURNING *;

-- name: LockSTKPushByTransaction :one
SELECT * FROM stk_pushes WHERE transaction_id = @transaction_id FOR NO KEY UPDATE;

-- name: GetSTKPushByTransaction :one
SELECT * FROM stk_pushes WHERE transaction_id = @transaction_id;

-- name: LatestSTKPushForRequest :one
SELECT * FROM stk_pushes
 WHERE payment_request_id = @payment_request_id AND created_at >= @since::timestamptz
 ORDER BY created_at DESC, id DESC
 LIMIT 1;
