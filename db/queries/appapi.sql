-- App-facing (/v1) reads. Every query is scoped by app_id, which the
-- handler takes ONLY from the authenticated API key.

-- name: ListTransactionsForApp :many
SELECT t.id, t.direction, t.kind, t.channel, t.status, t.amount, t.currency, t.provider_transaction_id,
       t.phone, t.account_number, t.charge_id, t.reverses_transaction_id,
       t.occurred_at, t.created_at, t.completed_at
  FROM transactions t
 WHERE t.app_id = @app_id
   AND (sqlc.narg(charge_id)::uuid IS NULL OR t.charge_id = sqlc.narg(charge_id))
   AND (sqlc.narg(since)::timestamptz IS NULL OR t.created_at >= sqlc.narg(since))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (t.created_at, t.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY t.created_at DESC, t.id DESC
 LIMIT @lim;

-- name: GetTransactionViewForApp :one
SELECT t.id, t.direction, t.kind, t.channel, t.status, t.amount, t.currency, t.provider_transaction_id,
       t.phone, t.account_number, t.charge_id, t.reverses_transaction_id,
       t.occurred_at, t.created_at, t.completed_at
  FROM transactions t
 WHERE t.app_id = @app_id AND t.id = @id;
