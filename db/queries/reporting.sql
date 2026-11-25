-- Read-only queries for the admin panel.
--
-- Lists use keyset pagination on (created_at, id) DESC: pass the last row's
-- values as cursor_at / cursor_id to get the next page. Every filter is
-- optional (NULL = no filter). Search patterns arrive already lower-cased and
-- LIKE-escaped from Go (reporting.likePattern).

-- name: ListChargesAdmin :many
SELECT c.id, c.app_id, a.account_prefix, a.name AS app_name, c.reference, c.description, c.amount,
       c.currency, c.account_number, c.phone, c.status, c.expires_at, c.succeeded_at, c.transaction_id,
       c.closed_at, c.created_at
  FROM charges c
  JOIN apps a ON a.id = c.app_id
 WHERE (sqlc.narg(app_id)::uuid IS NULL OR c.app_id = sqlc.narg(app_id))
   AND (sqlc.narg(status)::charge_status IS NULL OR c.status = sqlc.narg(status))
   AND (sqlc.narg(pattern)::text IS NULL
        OR c.account_number_norm = sqlc.narg(account_norm)::text
        OR lower(c.reference) LIKE sqlc.narg(pattern)
        OR lower(c.description) LIKE sqlc.narg(pattern))
   AND (sqlc.narg(created_from)::timestamptz IS NULL OR c.created_at >= sqlc.narg(created_from))
   AND (sqlc.narg(created_to)::timestamptz IS NULL OR c.created_at < sqlc.narg(created_to))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (c.created_at, c.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY c.created_at DESC, c.id DESC
 LIMIT @lim;

-- name: GetChargeAdmin :one
SELECT c.*, a.account_prefix, a.name AS app_name, a.status AS app_status
  FROM charges c
  JOIN apps a ON a.id = c.app_id
 WHERE c.id = @id;

-- name: ListTransactionsForChargeAdmin :many
-- Every transaction naming this charge: the one that paid it, duplicates,
-- failed prompts and a reversal.
SELECT * FROM transactions WHERE charge_id = @charge_id ORDER BY created_at, id;

-- name: ListTransactionsAdmin :many
SELECT t.id, t.app_id, a.account_prefix, t.charge_id, c.reference AS charge_reference, t.direction,
       t.kind, t.channel, t.status, t.amount, t.provider_transaction_id, t.phone, t.account_number,
       t.failure_reason, t.created_at, t.completed_at
  FROM transactions t
  LEFT JOIN apps a ON a.id = t.app_id
  LEFT JOIN charges c ON c.id = t.charge_id
 WHERE (sqlc.narg(app_id)::uuid IS NULL OR t.app_id = sqlc.narg(app_id))
   AND (sqlc.narg(charge_id)::uuid IS NULL OR t.charge_id = sqlc.narg(charge_id))
   AND (sqlc.narg(channel)::txn_channel IS NULL OR t.channel = sqlc.narg(channel))
   AND (sqlc.narg(status)::txn_status IS NULL OR t.status = sqlc.narg(status))
   AND (sqlc.narg(direction)::txn_direction IS NULL OR t.direction = sqlc.narg(direction))
   AND (sqlc.narg(receipt)::text IS NULL OR t.provider_transaction_id = sqlc.narg(receipt))
   AND (sqlc.narg(phone)::text IS NULL OR t.phone = sqlc.narg(phone))
   AND (sqlc.narg(created_from)::timestamptz IS NULL OR t.created_at >= sqlc.narg(created_from))
   AND (sqlc.narg(created_to)::timestamptz IS NULL OR t.created_at < sqlc.narg(created_to))
   AND (NOT @unmatched_only::boolean
        OR (t.app_id IS NULL AND t.direction = 'in' AND t.status = 'succeeded'))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (t.created_at, t.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY t.created_at DESC, t.id DESC
 LIMIT @lim;

-- name: ListUnmatchedTransactionsAdmin :many
-- Money that could not be routed to an app (unknown account prefix).
SELECT t.id, t.channel, t.status, t.amount, t.provider_transaction_id, t.phone, t.account_number,
       t.occurred_at, t.created_at, t.completed_at
  FROM transactions t
 WHERE t.app_id IS NULL AND t.direction = 'in' AND t.status = 'succeeded'
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (t.created_at, t.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY t.created_at DESC, t.id DESC
 LIMIT @lim;

-- name: GetTransactionAdmin :one
SELECT t.*, a.account_prefix, a.name AS app_name, a.status AS app_status, c.reference AS charge_reference
  FROM transactions t
  LEFT JOIN apps a ON a.id = t.app_id
  LEFT JOIN charges c ON c.id = t.charge_id
 WHERE t.id = @id;

-- name: ListTransactionEvents :many
SELECT * FROM transaction_events WHERE transaction_id = @transaction_id ORDER BY id;

-- name: ListAuditAdmin :many
SELECT a.id, a.occurred_at, a.actor_type, a.actor_id, a.action, a.entity_type, a.entity_id, a.app_id,
       ap.account_prefix, a.details
  FROM audit_log a
  LEFT JOIN apps ap ON ap.id = a.app_id
 WHERE (sqlc.narg(app_id)::uuid IS NULL OR a.app_id = sqlc.narg(app_id))
   AND (sqlc.narg(action_prefix)::text IS NULL OR a.action LIKE sqlc.narg(action_prefix))
   AND (sqlc.narg(actor_id)::text IS NULL OR a.actor_id = sqlc.narg(actor_id))
   AND (sqlc.narg(entity_id)::text IS NULL OR a.entity_id = sqlc.narg(entity_id))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (a.occurred_at, a.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::bigint))
 ORDER BY a.occurred_at DESC, a.id DESC
 LIMIT @lim;

-- name: CollectionsByDay :many
-- Successful incoming payments per Nairobi calendar day and app.
-- Unmatched money is still collected revenue: it is reported under the nil
-- UUID ("unassigned") rather than dropped.
SELECT ((t.completed_at AT TIME ZONE 'Africa/Nairobi')::date)::text AS day,
       coalesce(t.app_id, '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS app_id,
       sum(t.amount)::bigint AS total, count(*)::bigint AS payments
  FROM transactions t
 WHERE t.direction = 'in' AND t.kind = 'payment' AND t.status = 'succeeded'
   AND t.completed_at >= sqlc.arg(since)::timestamptz
 GROUP BY 1, 2
 ORDER BY 1, 2;

-- name: DashboardCounts :one
SELECT
  (SELECT count(*) FROM charges WHERE status = 'pending')::bigint AS charges_pending,
  -- "Today" is the Nairobi calendar day.
  (SELECT count(*) FROM charges
    WHERE status = 'succeeded'
      AND succeeded_at >= (date_trunc('day', now() AT TIME ZONE 'Africa/Nairobi') AT TIME ZONE 'Africa/Nairobi'))::bigint AS charges_succeeded_today,
  (SELECT coalesce(sum(amount), 0) FROM charges
    WHERE status = 'succeeded'
      AND succeeded_at >= (date_trunc('day', now() AT TIME ZONE 'Africa/Nairobi') AT TIME ZONE 'Africa/Nairobi'))::bigint AS charges_succeeded_today_amount,
  (SELECT count(*) FROM charges WHERE status = 'needs_review')::bigint AS charges_needs_review,
  (SELECT count(*) FROM transactions WHERE status = 'needs_review')::bigint AS transactions_needs_review,
  (SELECT count(*) FROM transactions WHERE app_id IS NULL AND direction = 'in' AND status = 'succeeded')::bigint AS unmatched_count,
  (SELECT coalesce(sum(amount), 0) FROM transactions WHERE app_id IS NULL AND direction = 'in' AND status = 'succeeded')::bigint AS unmatched_amount,
  (SELECT count(*) FROM transactions WHERE status = 'pending')::bigint AS pending_transactions,
  -- STK pushes a status check said succeeded but no receipt arrived: their
  -- transactions wait in needs_review for staff to enter the receipt.
  (SELECT count(*) FROM stk_pushes WHERE status = 'unknown')::bigint AS unknown_stk_pushes,
  -- Callbacks needing attention: still unprocessed 5 minutes after arrival
  -- (the worker is down or keeps failing), or processed with an error in
  -- the last 7 days that recorded no money (malformed body, foreign
  -- shortcode, ...). Notes on callbacks that did record money (duplicate,
  -- held unmatched) are not failures.
  (SELECT count(*) FROM mpesa_callbacks
    WHERE (processed_at IS NULL AND received_at < now() - interval '5 minutes')
       OR (error IS NOT NULL AND transaction_id IS NULL AND received_at > now() - interval '7 days'))::bigint AS unprocessed_callbacks,
  -- Money received in the last 30 days for an app that is not active. It
  -- was still recorded and forwarded (money rule 8), but staff should
  -- follow up.
  (SELECT count(*) FROM transactions t
     JOIN apps a ON a.id = t.app_id
    WHERE t.direction = 'in' AND t.status = 'succeeded'
      AND coalesce(t.completed_at, t.created_at) > now() - interval '30 days'
      AND a.status <> 'active')::bigint AS inactive_app_payments;
