-- Admin panel and operator CLI views of M-Pesa activity (read-only).

-- name: ListMpesaCallbacksAttention :many
-- Callback inbox for the admin panel. attention_only keeps callbacks that
-- need a look: not processed yet, or processed with an error and no money
-- recorded. (Notes on callbacks that did record money — duplicate receipt,
-- held in suspense — are not failures; those payments surface elsewhere.)
SELECT id, kind, remote_ip, received_at, processed_at, error, attempts, transaction_id
  FROM mpesa_callbacks
 WHERE (NOT @attention_only::boolean OR processed_at IS NULL OR (error IS NOT NULL AND transaction_id IS NULL))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (received_at, id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY received_at DESC, id DESC
 LIMIT @lim;

-- name: MpesaStatusCounts :one
-- Operator summary (`talksasa-pay admin mpesa status`).
SELECT
  (SELECT count(*) FROM stk_pushes WHERE status IN ('initiating', 'sent'))::bigint AS live_pushes,
  (SELECT count(*) FROM stk_pushes WHERE status = 'unknown')::bigint AS unknown_pushes,
  (SELECT count(*) FROM mpesa_callbacks WHERE processed_at IS NULL)::bigint AS unprocessed_callbacks,
  (SELECT count(*) FROM mpesa_callbacks
    WHERE error IS NOT NULL AND transaction_id IS NULL AND received_at > now() - interval '7 days')::bigint AS failed_callbacks_7d;
