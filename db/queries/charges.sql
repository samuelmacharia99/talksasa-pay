-- Charges: an app's request to collect an exact amount. Every app-facing
-- query is scoped by app_id, taken ONLY from the authenticated API key.

-- name: InsertCharge :one
-- Returns no row (pgx.ErrNoRows) when the idempotency key is already used;
-- a duplicate reference raises a unique violation on charges_app_id_reference_key.
INSERT INTO charges (id, app_id, reference, description, amount, metadata, account_number,
                     account_number_norm, account_generated, phone, hosted_token,
                     idempotency_key, request_hash, expires_at)
VALUES (@id, @app_id, @reference, sqlc.narg(description), @amount, sqlc.narg(metadata), @account_number,
        @account_number_norm, @account_generated, sqlc.narg(phone), @hosted_token,
        @idempotency_key, @request_hash, @expires_at)
ON CONFLICT (app_id, idempotency_key) DO NOTHING
RETURNING *;

-- name: GetChargeForApp :one
SELECT * FROM charges WHERE app_id = @app_id AND id = @id;

-- name: GetChargeInternal :one
-- Workers and admin only: not app-scoped.
SELECT * FROM charges WHERE id = @id;

-- name: GetChargeByIdempotencyKey :one
SELECT * FROM charges WHERE app_id = @app_id AND idempotency_key = @idempotency_key;

-- name: GetChargeByReference :one
SELECT * FROM charges WHERE app_id = @app_id AND reference = @reference;

-- name: LockCharge :one
SELECT * FROM charges WHERE app_id = @app_id AND id = @id FOR NO KEY UPDATE;

-- name: LockChargeInternal :one
SELECT * FROM charges WHERE id = @id FOR NO KEY UPDATE;

-- name: ListChargesForApp :many
SELECT * FROM charges
 WHERE app_id = @app_id
   AND (sqlc.narg(status)::charge_status IS NULL OR status = sqlc.narg(status))
   AND (sqlc.narg(reference)::text IS NULL OR reference = sqlc.narg(reference))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (created_at, id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY created_at DESC, id DESC
 LIMIT @lim;

-- name: MarkChargeSucceeded :one
-- pending | needs_review | expired | cancelled -> succeeded. charges_guard
-- checks the transaction (succeeded, receipted, same app and charge, exact
-- amount). No row when the charge already succeeded (or was reversed).
UPDATE charges
   SET status = 'succeeded', transaction_id = @transaction_id, succeeded_at = now()
 WHERE id = @id AND status IN ('pending', 'needs_review', 'expired', 'cancelled')
RETURNING *;

-- name: MarkChargeNeedsReview :one
UPDATE charges SET status = 'needs_review'
 WHERE id = @id AND status = 'pending'
RETURNING *;

-- name: MarkChargePending :one
-- Staff resolved a needs_review charge as not paid.
UPDATE charges SET status = 'pending'
 WHERE id = @id AND status = 'needs_review'
RETURNING *;

-- name: MarkChargeReversed :one
UPDATE charges SET status = 'reversed'
 WHERE id = @id AND status = 'succeeded'
RETURNING *;

-- name: CancelCharge :one
UPDATE charges SET status = 'cancelled', closed_at = now()
 WHERE app_id = @app_id AND id = @id AND status = 'pending'
RETURNING *;

-- name: ExpireDueCharges :many
-- Only pending charges expire (never while staff review one). SKIP LOCKED:
-- a charge being paid right now is left for the next run.
UPDATE charges
   SET status = 'expired', closed_at = now()
 WHERE id IN (
   SELECT c.id FROM charges c
    WHERE c.status = 'pending' AND c.expires_at <= now()
    ORDER BY c.expires_at
    LIMIT @batch_size
    FOR NO KEY UPDATE SKIP LOCKED)
RETURNING *;

-- name: GetChargeByHostedToken :one
-- Public pay page.
SELECT c.id, c.app_id, a.name AS app_name, a.status AS app_status, c.reference, c.description,
       c.amount, c.currency, c.account_number, c.status, c.expires_at, c.succeeded_at, c.created_at
  FROM charges c
  JOIN apps a ON a.id = c.app_id
 WHERE c.hosted_token = @hosted_token;

-- name: FindLiveChargesByAccount :many
-- Paybill matching (money rule 7): a live charge of this app with this
-- account number and exactly this amount. The caller acts only on exactly
-- one candidate.
SELECT * FROM charges
 WHERE app_id = @app_id AND account_number_norm = @account_number_norm AND amount = @amount
   AND status IN ('pending', 'needs_review')
 ORDER BY created_at, id
 LIMIT 2;

-- name: GetLatestSTKPushForCharge :one
SELECT * FROM stk_pushes WHERE charge_id = @charge_id ORDER BY created_at DESC, id DESC LIMIT 1;

-- name: ListSTKPushesForCharge :many
SELECT * FROM stk_pushes WHERE charge_id = @charge_id ORDER BY created_at DESC, id DESC LIMIT @lim;

-- name: ListLatestSTKPushesForCharges :many
-- The newest prompt of each charge on a list page (no N+1).
SELECT DISTINCT ON (charge_id) * FROM stk_pushes
 WHERE app_id = @app_id AND charge_id = ANY(@charge_ids::uuid[])
 ORDER BY charge_id, created_at DESC, id DESC;
